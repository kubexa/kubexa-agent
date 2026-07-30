package capability

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const (
	componentName = "capability-reporter"

	// Discovery is one or two API calls, so it can run often.
	defaultDiscoveryInterval = 5 * time.Minute
	// The SSAR sweep is the expensive part. It runs on this interval only as a
	// safety net for RBAC changes, which discovery cannot see.
	defaultSweepInterval = time.Hour
)

// Writer sends a message toward the gateway. Mirrors the state collector's
// Writer so the reporter can share the agent's outbound queue.
type Writer interface {
	Write(ctx context.Context, msg *agentv1.AgentMessage) error
}

// Options configures a Reporter.
type Options struct {
	Clientset         kubernetes.Interface
	Writer            Writer
	AgentMeta         *commonv1.AgentMetadata
	Logger            *logger.Logger
	Workers           int
	DiscoveryInterval time.Duration
	SweepInterval     time.Duration
}

// Reporter discovers the cluster's resource types and the agent's permission
// on each, and publishes a full snapshot to the gateway.
type Reporter struct {
	cs        kubernetes.Interface
	writer    Writer
	agentMeta *commonv1.AgentMetadata
	log       *logger.Logger
	workers   int

	discoveryInterval time.Duration
	sweepInterval     time.Duration

	state sweepState

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// sweepState remembers enough to decide whether the expensive probe is worth
// re-running. Kept separate from Reporter so the decision is testable without
// a cluster.
type sweepState struct {
	lastFingerprint string
	lastSweep       time.Time
	ran             bool
}

// needsSweep is true on the first run, whenever the set of GVRs changed, and
// whenever the safety interval has elapsed. The last case is not redundant:
// an operator widening the ClusterRole changes no GVR, so a fingerprint-only
// rule would never notice the new grant.
func (s sweepState) needsSweep(fingerprint string, now time.Time, safety time.Duration) bool {
	if !s.ran {
		return true
	}
	if fingerprint != s.lastFingerprint {
		return true
	}
	return now.Sub(s.lastSweep) >= safety
}

// NewReporter validates options and returns a Reporter.
func NewReporter(opts Options) (*Reporter, error) {
	if opts.Clientset == nil {
		return nil, errors.New("capability: clientset is required")
	}
	if opts.Writer == nil {
		return nil, errors.New("capability: writer is required")
	}
	log := opts.Logger
	if log == nil {
		log = logger.New(componentName)
	}
	r := &Reporter{
		cs:                opts.Clientset,
		writer:            opts.Writer,
		agentMeta:         opts.AgentMeta,
		log:               log.With("component", componentName),
		workers:           opts.Workers,
		discoveryInterval: opts.DiscoveryInterval,
		sweepInterval:     opts.SweepInterval,
	}
	if r.discoveryInterval <= 0 {
		r.discoveryInterval = defaultDiscoveryInterval
	}
	if r.sweepInterval <= 0 {
		r.sweepInterval = defaultSweepInterval
	}
	return r, nil
}

// Name identifies this component in the agent's collector registry.
func (r *Reporter) Name() string { return componentName }

// Start runs an immediate refresh, then one every discovery interval.
func (r *Reporter) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// The first refresh stands in for "on handshake": the agent connects,
		// collectors start, and the catalog follows without the connection
		// having waited on a several-hundred-GVR sweep.
		r.refresh(runCtx)

		ticker := time.NewTicker(r.discoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				r.refresh(runCtx)
			}
		}
	}()
	return nil
}

// Stop cancels the refresh loop and waits for it to finish.
func (r *Reporter) Stop(_ context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	return nil
}

func (r *Reporter) refresh(ctx context.Context) {
	gvrs, failedGroups, err := Discover(r.cs.Discovery())
	if err != nil {
		r.log.Error("capability discovery failed", logger.F("error", err.Error()))
		return
	}
	if len(failedGroups) > 0 {
		r.log.Warn("some API groups were unreachable",
			logger.F("groups", failedGroups),
			logger.F("discovered", len(gvrs)),
		)
	}

	fingerprint := Fingerprint(gvrs)
	now := time.Now().UTC()
	if !r.state.needsSweep(fingerprint, now, r.sweepInterval) {
		return
	}

	capabilities := Probe(ctx, r.cs.AuthorizationV1(), gvrs, r.workers)
	if ctx.Err() != nil {
		return
	}

	// The sweep state only advances once the catalog has actually reached the
	// queue. A message that never reached the queue is not a completed sweep:
	// recording one anyway would leave the cluster showing no catalog until
	// the hourly safety sweep, over a transient hiccup like a full queue at
	// agent startup.
	if err := r.publish(ctx, capabilities, failedGroups, fingerprint, now); err != nil {
		r.log.Error("publish resource catalog failed", logger.F("error", err.Error()))
		return
	}
	r.state = sweepState{lastFingerprint: fingerprint, lastSweep: now, ran: true}

	r.log.Info("published resource catalog",
		logger.F("resources", len(capabilities)),
		logger.F("failed_groups", len(failedGroups)),
	)
}

// publish turns a probe result into a wire message and writes it. It has no
// side effect on sweep state -- refresh only advances that after publish
// succeeds.
func (r *Reporter) publish(
	ctx context.Context,
	capabilities []Capability,
	failedGroups []string,
	fingerprint string,
	at time.Time,
) error {
	catalog := buildCatalog(capabilities, failedGroups, fingerprint, at)
	msg := &agentv1.AgentMessage{
		MessageId: uuid.NewString(),
		Meta:      r.metaSnapshot(at),
		Payload:   &agentv1.AgentMessage_Catalog{Catalog: catalog},
	}
	return r.writer.Write(ctx, msg)
}

// buildCatalog turns a probe result into the wire message. collectedAt is unix
// MILLIseconds, matching catalog.proto's documented unit.
func buildCatalog(
	caps []Capability,
	failedGroups []string,
	fingerprint string,
	at time.Time,
) *agentv1.ResourceCatalog {
	entries := make([]*agentv1.ResourceCapability, 0, len(caps))
	for _, c := range caps {
		entries = append(entries, &agentv1.ResourceCapability{
			Group:       c.Group,
			Version:     c.Version,
			Resource:    c.Resource,
			Kind:        c.Kind,
			Namespaced:  c.Namespaced,
			CanList:     c.CanList,
			CanWatch:    c.CanWatch,
			ProbeFailed: c.ProbeFailed,
		})
	}
	return &agentv1.ResourceCatalog{
		Fingerprint:  fingerprint,
		CollectedAt:  at.UnixMilli(),
		Entries:      entries,
		FailedGroups: failedGroups,
	}
}

func (r *Reporter) metaSnapshot(ts time.Time) *commonv1.AgentMetadata {
	meta, _ := proto.Clone(r.agentMeta).(*commonv1.AgentMetadata)
	if meta == nil {
		meta = &commonv1.AgentMetadata{}
	}
	meta.Timestamp = ts.UnixMilli()
	return meta
}
