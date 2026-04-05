package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/kubexa/kubexa-agent/internal/config"
	kubegrpc "github.com/kubexa/kubexa-agent/internal/grpc"
	"github.com/kubexa/kubexa-agent/internal/k8s"
)

// Agent wires Kubernetes informers (pod watch), log streaming, and gRPC reporters.
type Agent struct {
	cfg *config.Config
}

// NewAgent constructs an agent with loaded configuration.
func NewAgent(cfg *config.Config) *Agent {
	return &Agent{cfg: cfg}
}

// Run blocks until ctx is cancelled. It connects to the backend, starts the reconnect loop,
// runs pod and log gRPC streams, and drives LogStreamer from PodWatcher events.
func (a *Agent) Run(ctx context.Context) error {
	k8sClient, err := k8s.NewClient()
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	grpcClient := kubegrpc.NewClient(a.cfg.GRPC, a.cfg.Backend, a.cfg.ClusterID)
	reconn := kubegrpc.NewReconnector(a.cfg.GRPC, grpcClient)
	go reconn.Run(ctx)

	if err := grpcClient.Connect(ctx); err != nil {
		return fmt.Errorf("grpc connect: %w", err)
	}
	defer func() {
		if closeErr := grpcClient.Close(); closeErr != nil {
			log.Warn().Err(closeErr).Msg("grpc client close")
		}
	}()

	spillPod, spillLog, cleanupSpills, err := buildOutboundSpillovers(a.cfg)
	if err != nil {
		return fmt.Errorf("outbound buffers: %w", err)
	}
	defer cleanupSpills()

	// Child context stops reporters and relays if Run returns before the process signal fires.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	eventBuf := a.cfg.Logs.BufferSize
	if eventBuf < 256 {
		eventBuf = 256
	}

	// Informers must cover every namespace in logs.targets; otherwise deletes there never arrive
	// and the log bridge cannot run StopPodStream.
	watchCfg := watchWithLogNamespaces(a.cfg)
	podWatcher := k8s.NewPodWatcher(k8sClient, watchCfg, eventBuf)
	logStreamer := k8s.NewLogStreamer(k8sClient, a.cfg.Logs)

	podReporter := kubegrpc.NewPodReporter(grpcClient, a.cfg.ClusterID, spillPod)
	logReporter := kubegrpc.NewLogReporter(grpcClient, a.cfg.ClusterID, spillLog)

	// Exactly one consumer of podWatcher.Events(); fan-out duplicates to log bridge + gRPC.
	relay := make(chan *k8s.PodEvent, eventBuf)
	go a.relayPodEvents(runCtx, podWatcher.Events(), relay, logStreamer)
	go podReporter.Run(runCtx, relay)

	go logReporter.Run(runCtx, logStreamer.Logs())

	kubegrpc.RunAgentControl(runCtx, grpcClient, k8sClient)

	if err := podWatcher.Start(ctx); err != nil {
		return fmt.Errorf("pod watcher: %w", err)
	}

	if err := logStreamer.Start(runCtx); err != nil {
		log.Warn().Err(err).Msg("log streamer initial start completed with errors")
	}

	<-ctx.Done()

	logStreamer.Stop()
	return ctx.Err()
}

// watchWithLogNamespaces adds namespaces from logs.targets so informers emit lifecycle events
// (including DELETED) for pods you tail, even if watch.namespaces omitted them.
func watchWithLogNamespaces(cfg *config.Config) config.WatchConfig {
	w := cfg.Watch
	out := make([]config.NamespaceSelector, len(w.Namespaces))
	copy(out, w.Namespaces)

	seen := make(map[string]struct{}, len(out))
	for _, ns := range out {
		seen[ns.Namespace] = struct{}{}
	}
	if cfg.Logs.Enabled {
		for _, t := range cfg.Logs.Targets {
			if t.Namespace == "" {
				continue
			}
			if _, ok := seen[t.Namespace]; ok {
				continue
			}
			seen[t.Namespace] = struct{}{}
			out = append(out, config.NamespaceSelector{
				Namespace:     t.Namespace,
				LabelSelector: "",
			})
		}
	}
	w.Namespaces = out
	return w
}

// relayPodEvents reads the informer channel once per event, updates log tails, then forwards
// the same event to the pod gRPC reporter. (Two goroutines must not read the same chan.)
func (a *Agent) relayPodEvents(
	ctx context.Context,
	in <-chan *k8s.PodEvent,
	out chan<- *k8s.PodEvent,
	ls *k8s.LogStreamer,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-in:
			if ev == nil {
				continue
			}
			a.applyLogBridge(ctx, ls, ev)
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}

// applyLogBridge maps one pod lifecycle event to log stream start/stop.
func (a *Agent) applyLogBridge(ctx context.Context, ls *k8s.LogStreamer, ev *k8s.PodEvent) {
	if !a.cfg.Logs.Enabled || ev.Pod == nil {
		return
	}
	switch ev.Type {
	case k8s.EventAdded, k8s.EventModified:
		if ok, containers := ls.ShouldStream(ev.Pod); ok {
			ls.StartPodStream(ctx, ev.Pod, containers)
		}
	case k8s.EventDeleted:
		ls.StopPodStream(ev.Pod.Namespace, ev.Pod.Name)
	}
}

// RunWait behaves like Run but maps context.Canceled to nil for tidy main exit codes.
func (a *Agent) RunWait(ctx context.Context) error {
	err := a.Run(ctx)
	if err != nil && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
