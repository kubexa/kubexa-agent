package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/stream"
	"github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

// fakeEffectiveConfigSource stands in for the stream manager during boot: it
// answers with a config and decides whether -- and when -- the first
// handshake settles.
type fakeEffectiveConfigSource struct {
	settled   chan struct{}
	effective *config.Config
}

func (f *fakeEffectiveConfigSource) ConfigSettled() <-chan struct{} { return f.settled }
func (f *fakeEffectiveConfigSource) EffectiveConfig() *config.Config {
	return f.effective
}

func bootTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Agent.TenantToken = "tenant-secret"
	cfg.Agent.AgentID = "agent-1"
	cfg.Agent.ClusterID = "cluster-1"
	cfg.Gateway.Address = "127.0.0.1:0"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.Validate: %v", err)
	}
	return cfg
}

// TestCollectorsAreBuiltFromTheEffectiveConfig is C1 itself: the collectors
// were built from the FILE config, and the platform's managed set arrives at
// the handshake, after that. A restart did not rescue it -- the same
// ordering repeated -- so four of the five managed blocks changed nothing
// while the agent reported them applied.
func TestCollectorsAreBuiltFromTheEffectiveConfig(t *testing.T) {
	file := bootTestConfig(t)
	effective, err := config.ApplyBlocks(file, map[string][]byte{
		"collect.logs": []byte("enabled: true\ntail_lines: 42\n"),
	})
	if err != nil {
		t.Fatalf("ApplyBlocks: %v", err)
	}
	settled := make(chan struct{})
	close(settled)
	src := &fakeEffectiveConfigSource{settled: settled, effective: effective}

	var built *config.Config
	var reported *config.Config
	collectors, used, err := startCollectors(
		context.Background(), src, time.Second,
		func(c *config.Config) ([]Collector, error) {
			built = c
			return nil, nil
		},
		func(c *config.Config) { reported = c },
		logger.New("boot-test"),
	)
	if err != nil {
		t.Fatalf("startCollectors: %v", err)
	}
	if len(collectors) != 0 {
		t.Fatalf("collectors = %d, want 0 from this stub builder", len(collectors))
	}
	if built == nil || built.Collect.Logs.TailLines != 42 {
		t.Fatalf("collectors were built from the wrong config: tail_lines = %v, want 42 "+
			"(the managed block the platform pushed)", built.Collect.Logs.TailLines)
	}
	if used != built {
		t.Fatalf("startCollectors returned a different config than it built from")
	}
	// The stream manager has to be told what was built, or the heartbeat
	// cannot tell "applied" from "applied but not in effect".
	if reported != built {
		t.Fatalf("the config the collectors were built from was not reported to the stream manager")
	}
}

// TestBootFallsBackToTheFileConfigWhenNoManagedSetArrives: an agent whose
// gateway is unreachable must still collect. The wait is bounded and the
// fallback is the operator's own file config -- never an empty one, and
// never a hang.
func TestBootFallsBackToTheFileConfigWhenNoManagedSetArrives(t *testing.T) {
	file := bootTestConfig(t)
	// Never closed: the gateway never answers a handshake.
	src := &fakeEffectiveConfigSource{settled: make(chan struct{}), effective: file}

	var built *config.Config
	start := time.Now()
	_, _, err := startCollectors(
		context.Background(), src, 50*time.Millisecond,
		func(c *config.Config) ([]Collector, error) {
			built = c
			return nil, nil
		},
		func(*config.Config) {},
		logger.New("boot-test"),
	)
	if err != nil {
		t.Fatalf("startCollectors: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("boot waited %s for a managed set that never came", elapsed)
	}
	if built != file {
		t.Fatal("collectors must be built from the file config when no managed set arrives")
	}
}

// TestAFailedBuildReportsNoCollectorConfig: report tells the stream manager
// what the collectors were built FROM. If the build failed there are no
// collectors, and reporting a config anyway would have the heartbeat compare
// later revisions against something nothing is running.
func TestAFailedBuildReportsNoCollectorConfig(t *testing.T) {
	settled := make(chan struct{})
	close(settled)
	src := &fakeEffectiveConfigSource{settled: settled, effective: bootTestConfig(t)}

	reported := false
	_, _, err := startCollectors(
		context.Background(), src, time.Second,
		func(*config.Config) ([]Collector, error) { return nil, errors.New("boom") },
		func(*config.Config) { reported = true },
		logger.New("boot-test"),
	)
	if err == nil {
		t.Fatal("startCollectors must return the builder's error")
	}
	if reported {
		t.Fatal("a failed build must not report a collector config")
	}
}

// TestReconcilerHolderIsNotReadyBeforeCollectorsExist: the holder exists
// because collectors depend on the stream manager and the manager needs the
// reconciler they carry -- "manager first, then collectors" is a cycle. A
// reconcile landing inside that window must return a clear error: the
// platform retries on an error, and it cannot retry a lie. A nil
// dereference, or a silent success, are the two ways this goes wrong.
func TestReconcilerHolderIsNotReadyBeforeCollectorsExist(t *testing.T) {
	h := &reconcilerHolder{}

	err := h.Reconcile(context.Background(), []k8sresource.Descriptor{})
	if err == nil {
		t.Fatal("a reconcile before the collectors exist must not report success")
	}
	if !errors.Is(err, errReconcilerNotReady) {
		t.Fatalf("error = %v, want errReconcilerNotReady so the caller can tell it apart", err)
	}
}

// countingReconciler records what the holder forwards once it is populated.
type countingReconciler struct {
	calls int
	last  []k8sresource.Descriptor
	err   error
}

func (c *countingReconciler) Reconcile(_ context.Context, desired []k8sresource.Descriptor) error {
	c.calls++
	c.last = desired
	return c.err
}

// TestReconcilerHolderForwardsOnceTheCollectorExists closes the other half:
// the holder must be a pass-through, not a swallow-all, or demand-driven
// watches would stop converging for the life of the process.
func TestReconcilerHolderForwardsOnceTheCollectorExists(t *testing.T) {
	h := &reconcilerHolder{}
	real := &countingReconciler{err: errors.New("from the collector")}
	h.Set(real)

	desired := []k8sresource.Descriptor{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}}
	err := h.Reconcile(context.Background(), desired)
	if real.calls != 1 {
		t.Fatalf("Reconcile calls = %d, want 1", real.calls)
	}
	if len(real.last) != 1 {
		t.Fatalf("the desired set was not forwarded: %v", real.last)
	}
	if !errors.Is(err, real.err) {
		t.Fatalf("error = %v, want the collector's own error", err)
	}
}

// TestReconcilerHolderResolvedToNoneSucceedsSilently: an agent with
// collect.state disabled builds no state collector, so nothing satisfies
// stream.WatchReconciler. Before this task the manager held a nil reconciler
// and skipped the reconcile; the holder must keep that behaviour rather than
// answering "not ready" forever, which would log an error on every config
// update for an agent that is behaving exactly as configured.
func TestReconcilerHolderResolvedToNoneSucceedsSilently(t *testing.T) {
	h := &reconcilerHolder{}
	h.Set(nil)

	if err := h.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("Reconcile = %v, want nil when there is no state collector at all", err)
	}
}

// TestScrapeHealthHolderReportsNothingBeforeTheCollectorExists: an empty
// list from this holder must mean "not measured", which is exactly what the
// heartbeat's scrape_targets field already means when absent -- never a
// fabricated all-healthy list for collectors that do not exist yet.
func TestScrapeHealthHolderReportsNothingBeforeTheCollectorExists(t *testing.T) {
	h := &scrapeHealthHolder{}
	if got := h.ScrapeTargetHealth(); got != nil {
		t.Fatalf("ScrapeTargetHealth = %v, want nil before the metrics collector exists", got)
	}
}

// TestSweeperHolderDropsANudgeBeforeTheReporterExists: a policy swap inside
// the boot window has nothing to nudge, and the capability reporter built
// afterwards sweeps on its own first tick anyway. It must not panic.
func TestSweeperHolderDropsANudgeBeforeTheReporterExists(t *testing.T) {
	h := &sweeperHolder{}
	h.RequestSweep()

	real := &countingSweeper{}
	h.Set(real)
	h.RequestSweep()
	if real.calls != 1 {
		t.Fatalf("RequestSweep calls = %d, want 1 once the reporter exists", real.calls)
	}
}

// Compile-time proof that each holder still satisfies the interface the
// stream manager is constructed with. A holder that stopped satisfying it
// would fail at the stream.New call in serve(), but only when this package
// is built -- these keep the reason visible here.
var (
	_ stream.WatchReconciler    = (*reconcilerHolder)(nil)
	_ stream.ScrapeHealthSource = (*scrapeHealthHolder)(nil)
	_ sweepRequester            = (*sweeperHolder)(nil)
)

// TestBootBuildsNothingWhenTheContextEndsDuringTheWait: a SIGTERM inside the
// boot wait, or a server that has already failed into the run group, must
// not be answered by starting informers for a process that is shutting down.
func TestBootBuildsNothingWhenTheContextEndsDuringTheWait(t *testing.T) {
	src := &fakeEffectiveConfigSource{settled: make(chan struct{}), effective: bootTestConfig(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	builds := 0
	_, _, err := startCollectors(
		ctx, src, time.Second,
		func(*config.Config) ([]Collector, error) {
			builds++
			return nil, nil
		},
		func(*config.Config) {},
		logger.New("boot-test"),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled so run() reads it as a clean stop", err)
	}
	if builds != 0 {
		t.Fatalf("build calls = %d, want 0 once the context is done", builds)
	}
}
