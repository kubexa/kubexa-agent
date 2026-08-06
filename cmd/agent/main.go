// Command agent is the kubexa-agent process entrypoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/rest"

	"github.com/kubexa/kubexa-agent/internal/capability"
	"github.com/kubexa/kubexa-agent/internal/collector/logs"
	metricscollector "github.com/kubexa/kubexa-agent/internal/collector/metrics"
	"github.com/kubexa/kubexa-agent/internal/collector/state"
	"github.com/kubexa/kubexa-agent/internal/health"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/k8s/k8sconfig"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/query"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/internal/stream"
	"github.com/kubexa/kubexa-agent/pkg/buildinfo"
	"github.com/kubexa/kubexa-agent/pkg/config"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const (
	defaultConfigPath      = "/config/config.yaml"
	defaultShutdownTimeout = 30 * time.Second
)

// Collector is a long-running data collection component (logs, state, metrics).
// Implementations will be wired in later steps.
type Collector interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Name() string
}

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", defaultConfigPath, "path to agent config YAML")
	devFlag := flag.Bool("dev", false, "enable local development mode")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	devMode := *devFlag
	if devMode {
		cfg.Log.Level = "debug"
		cfg.Log.Format = "console"
	}

	level, err := logger.ParseLevel(cfg.Log.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse log level: %v\n", err)
		return 1
	}

	rootLog := logger.New("agent",
		logger.WithLevel(level),
		logger.WithDevelopment(cfg.Log.Format == "console"),
		logger.WithAgentID(cfg.Agent.AgentID),
		logger.WithClusterID(cfg.Agent.ClusterID),
	)

	if devMode {
		printBanner(os.Stdout)
	}
	rootLog.Info("starting kubexa-agent",
		logger.F("version", buildinfo.Version),
		logger.F("commit", buildinfo.Commit),
		logger.F("build_time", buildinfo.BuildTime),
		logger.F("dev", devMode),
		logger.F("config", *configPath),
	)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rootCtx = logger.NewContext(rootCtx, rootLog)

	if err := serve(rootCtx, cfg, devMode, rootLog); err != nil {
		if isContextClosed(err) {
			rootLog.Info("agent stopped")
			return 0
		}
		rootLog.Err(err).Error("agent exited with error")
		return 1
	}
	rootLog.Info("agent stopped")
	return 0
}

func serve(parentCtx context.Context, cfg *config.Config, devMode bool, log *logger.Logger) error {
	shutdownTimeout := defaultShutdownTimeout

	mainReg := prometheus.NewRegistry()

	kube, err := initKubernetes(parentCtx, cfg, devMode, log)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	log.Info("kubernetes client ready")

	if err := cfg.EnsureClusterID(parentCtx, kube); err != nil {
		return fmt.Errorf("cluster id: %w", err)
	}
	log = log.With("cluster_id", cfg.Agent.ClusterID)
	parentCtx = logger.NewContext(parentCtx, log)

	agentMetrics, err := metrics.New(mainReg, buildinfo.Version, cfg.Agent.ClusterID, cfg.Agent.AgentID)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	kube.EnableMetrics(agentMetrics.K8s())

	q, err := queue.New(&cfg.Buffer, logger.New("queue", logger.WithAgentID(cfg.Agent.AgentID)), agentMetrics.Queue())
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	log.Info("queue initialized", logger.F("spill_dir", cfg.Buffer.SpillDir))

	log.Info("collection settings",
		logger.F("logs_enabled", cfg.Collect.Logs.Enabled),
		logger.F("state_enabled", cfg.Collect.State.Enabled),
		logger.F("metrics_enabled", cfg.Collect.Metrics.Enabled),
	)

	// Compiled once and shared: the executor enforces it per request, and the
	// capability reporter publishes its per-GVR verdict to the UI.
	queryPolicy, err := policy.Compile(cfg)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query policy: %w", err)
	}

	// A wildcard rule permits every resource, secrets among them. Paired with
	// visible Secret values it is the widest read policy the agent can hold,
	// and an operator must not have to discover that from a screen.
	if ids := queryPolicy.UnredactedWildcardRuleIDs(); len(ids) > 0 {
		log.Warn("live query policy permits every resource and Secret values are not redacted",
			logger.F("rules", strings.Join(ids, ",")),
			logger.F("remedy", "set query.redact_secrets: true, or name resources explicitly"),
		)
	}

	// The live-query path gets its own client pair for the same reason the
	// capability probe does: an ad-hoc user query must not drain the
	// rate-limit token bucket the informers share. Zero QPS/burst means the
	// same budget as the main client, but through a SEPARATE limiter.
	queryClients, err := k8s.NewQueryClients(&k8sconfig.Config{}, 0, 0)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query clients: %w", err)
	}

	// Built even when query.enabled is false: the policy answers POLICY_DENIED,
	// which is a diagnosis. A nil responder would just drop the query.
	queryExecutor, err := query.New(query.Options{
		Clients:    *queryClients,
		Policy:     queryPolicy,
		Logger:     logger.New("query", logger.WithAgentID(cfg.Agent.AgentID)),
		Registerer: mainReg,
	})
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query executor: %w", err)
	}

	collectors, err := buildCollectors(cfg, kube, q, mainReg, log, queryPolicy)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("collectors: %w", err)
	}

	// The state collector, if enabled, also satisfies stream.WatchReconciler
	// (its Reconcile method converges demand-driven informers on the
	// gateway's watch config). Found by type assertion rather than a second
	// return value from buildCollectors so that "which collector applies
	// gateway config" stays a property of the collector's own type, not
	// something the caller has to track separately.
	var reconciler stream.WatchReconciler
	for _, coll := range collectors {
		if r, ok := coll.(stream.WatchReconciler); ok {
			reconciler = r
			break
		}
	}

	streamMgr, err := stream.New(
		cfg,
		q,
		logger.New("stream", logger.WithAgentID(cfg.Agent.AgentID)),
		agentMetrics.Stream(),
		agentMetrics.Connection(),
		reconciler,
		queryExecutor,
	)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("stream manager: %w", err)
	}
	log.Info("stream manager initialized")

	healthAddr := cfg.Observability.HealthAddr
	metricsAddr := cfg.Observability.MetricsAddr
	if devMode {
		healthAddr = bindLocalhost(healthAddr)
		metricsAddr = bindLocalhost(metricsAddr)
	}

	healthSrv := health.New(health.HealthConfig{Addr: healthAddr}, logger.New("health"), agentMetrics.Health())
	healthSrv.Register(health.NewK8sChecker(kube))
	healthSrv.Register(health.NewQueueChecker(q, 0))
	healthSrv.Register(health.NewStreamChecker(streamMgr))
	for _, coll := range collectors {
		if ready, ok := coll.(health.StateWatcherReady); ok {
			healthSrv.Register(health.NewStateWatcherChecker(ready))
		}
	}

	metricsSrv := metrics.NewServer(metricsAddr, mainReg, logger.New("metrics"))

	g, ctx := errgroup.WithContext(parentCtx)

	for _, c := range collectors {
		c := c
		g.Go(func() error {
			log.Info("starting collector", logger.F("collector", c.Name()))
			if err := c.Start(ctx); err != nil {
				return fmt.Errorf("collector %s start: %w", c.Name(), err)
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}

	g.Go(func() error {
		log.Info("starting health server", logger.F("addr", healthAddr))
		if err := healthSrv.Start(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("health server: %w", err)
		}
		log.Info("health server stopped")
		return nil
	})

	g.Go(func() error {
		log.Info("starting metrics server", logger.F("addr", metricsAddr))
		if err := metricsSrv.Run(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("metrics server: %w", err)
		}
		log.Info("metrics server stopped")
		return nil
	})

	g.Go(func() error {
		log.Info("starting stream manager")
		if err := streamMgr.Run(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("stream manager: %w", err)
		}
		log.Info("stream manager stopped")
		return nil
	})

	log.Info("agent ready",
		logger.F("health_addr", healthAddr),
		logger.F("metrics_addr", metricsAddr),
		logger.F("gateway", cfg.Gateway.Address),
	)

	runErr := g.Wait()

	log.Info("shutting down", logger.F("timeout", shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(parentCtx, shutdownTimeout)
	defer cancel()
	if errors.Is(shutdownCtx.Err(), context.Canceled) {
		var cancelTimeout context.CancelFunc
		shutdownCtx, cancelTimeout = context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelTimeout()
	}

	if err := stopCollectors(shutdownCtx, collectors, log); err != nil {
		log.Warn("collector shutdown", logger.F("error", err))
	}

	log.Info("closing queue")
	if err := q.Close(); err != nil && !isContextClosed(err) {
		log.Warn("queue close", logger.F("error", err))
	}

	if runErr != nil && !isContextClosed(runErr) {
		return runErr
	}
	return nil
}

func buildCollectors(
	cfg *config.Config,
	kube k8s.Client,
	q queue.Queue,
	reg prometheus.Registerer,
	log *logger.Logger,
	queryPolicy *policy.Policy,
) ([]Collector, error) {
	var collectors []Collector

	if cfg.Collect.Logs.Enabled {
		logColl, err := logs.New(logs.Options{
			Config: logs.ConfigFromRoot(cfg),
			Kube:   kube,
			Queue:  q,
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger:     logger.New("log-collector", logger.WithAgentID(cfg.Agent.AgentID)),
			Registerer: reg,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, logColl)
	}

	if cfg.Collect.State.Enabled {
		stateColl, err := state.New(state.Options{
			Config: state.ConfigFromRoot(cfg),
			Kube:   kube,
			Queue:  q,
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger:     logger.New("state-watcher", logger.WithAgentID(cfg.Agent.AgentID)),
			Registerer: reg,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, stateColl)

		// Zero QPS/burst means "take the same budget as the main client"
		// (k8sconfig.DefaultQPS/DefaultBurst) while still getting a SEPARATE
		// rate limiter — which is the whole point of a second clientset here:
		// a several-hundred-GVR sweep must not drain the token bucket the
		// informers share. Throttling the sweep itself buys nothing. An
		// earlier 5 QPS / 10 burst made a ~400-review sweep take over a
		// minute and filled the log with client-side throttling warnings.
		probeCS, err := k8s.NewProbeClientset(&k8sconfig.Config{}, 0, 0)
		if err != nil {
			return nil, err
		}
		capReporter, err := capability.NewReporter(capability.Options{
			Clientset: probeCS,
			Writer:    state.NewQueueWriter(q, state.ConfigFromRoot(cfg).WriteTimeout),
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger: logger.New("capability-reporter", logger.WithAgentID(cfg.Agent.AgentID)),
			Policy: queryPolicy,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, capReporter)
	}

	if cfg.Collect.Metrics.Enabled {
		metricsColl, err := metricscollector.New(metricscollector.Options{
			Config: metricscollector.ConfigFromRoot(cfg),
			Kube:   kube,
			Queue:  q,
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger:     logger.New("metrics-scraper", logger.WithAgentID(cfg.Agent.AgentID)),
			Registerer: reg,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, metricsColl)
	}

	return collectors, nil
}

func initKubernetes(ctx context.Context, cfg *config.Config, devMode bool, log *logger.Logger) (k8s.Client, error) {
	kube, err := k8s.New(&k8sconfig.Config{}, logger.New("k8s"))
	if err != nil {
		return nil, err
	}

	inCluster := runningInCluster()
	if inCluster && !devMode {
		if err := kube.Ready(ctx); err != nil {
			return nil, fmt.Errorf("in-cluster readiness: %w", err)
		}
		return kube, nil
	}

	if err := kube.Ready(ctx); err != nil {
		if inCluster {
			return nil, fmt.Errorf("in-cluster readiness: %w", err)
		}
		log.Warn("kubernetes readiness check failed (continuing outside cluster)",
			logger.F("error", err),
			logger.F("dev", devMode),
		)
	}
	return kube, nil
}

func runningInCluster() bool {
	_, err := rest.InClusterConfig()
	return err == nil
}

func stopCollectors(ctx context.Context, collectors []Collector, log *logger.Logger) error {
	var first error
	for i := len(collectors) - 1; i >= 0; i-- {
		c := collectors[i]
		log.Info("stopping collector", logger.F("collector", c.Name()))
		if err := c.Stop(ctx); err != nil {
			if first == nil && !isContextClosed(err) {
				first = fmt.Errorf("collector %s: %w", c.Name(), err)
			}
			log.Warn("collector stop failed",
				logger.F("collector", c.Name()),
				logger.F("error", err),
			)
		}
	}
	return first
}

func bindLocalhost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if len(addr) > 0 && addr[0] == ':' {
			return "127.0.0.1" + addr
		}
		return "127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

func isContextClosed(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func printBanner(w interface{ Write([]byte) (int, error) }) {
	_, _ = fmt.Fprintf(w, `
'##:::'##:'##::::'##:'########::'########:'##::::'##::::'###::::
 ##::'##:: ##:::: ##: ##.... ##: ##.....::. ##::'##::::'## ##:::
 ##:'##::: ##:::: ##: ##:::: ##: ##::::::::. ##'##::::'##:. ##::
 #####:::: ##:::: ##: ########:: ######:::::. ###::::'##:::. ##:
 ##. ##::: ##:::: ##: ##.... ##: ##...:::::: ## ##::: #########:
 ##:. ##:: ##:::: ##: ##:::: ##: ##:::::::: ##:. ##:: ##.... ##:
 ##::. ##:. #######:: ########:: ########: ##:::. ##: ##:::: ##:
..::::..:::.......:::........:::........::..:::::..::..:::::..::

%s (%s) %s

`, buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime)
}
