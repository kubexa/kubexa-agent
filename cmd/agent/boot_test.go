package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

// errFakeNotStarted mirrors, verbatim, what *state.Collector's reconcile
// returns while its runCtx is nil (internal/collector/state/reconciler.go).
var errFakeNotStarted = errors.New("reconcile: collector has not been started")

// countingReconciler is both a Collector and a stream.WatchReconciler,
// because the real one is: *state.Collector carries the reconciler AND is
// started through the collector list.
//
// It REFUSES every reconcile until Start has run, which is the contract the
// real collector actually has -- Start is what sets the runCtx its informers
// are parented on. An earlier version of this double accepted anything, and
// that is precisely what let a replay that production refuses pass five
// tests: the fake and the code agreed with each other while disagreeing with
// the collector. calls counts every attempt; applied counts only the ones
// that reached a started collector.
type countingReconciler struct {
	started bool
	calls   int
	applied int
	last    []k8sresource.Descriptor
	err     error
}

func (c *countingReconciler) Name() string                { return "fake-state-collector" }
func (c *countingReconciler) Stop(context.Context) error  { return nil }
func (c *countingReconciler) Start(context.Context) error { c.started = true; return nil }

func (c *countingReconciler) Reconcile(_ context.Context, desired []k8sresource.Descriptor) error {
	c.calls++
	c.last = desired
	if !c.started {
		return errFakeNotStarted
	}
	c.applied++
	return c.err
}

// plainCollector satisfies Collector and nothing else -- the log collector's
// shape, for the wiring test.
type plainCollector struct{}

func (plainCollector) Name() string                { return "fake-log-collector" }
func (plainCollector) Start(context.Context) error { return nil }
func (plainCollector) Stop(context.Context) error  { return nil }

// TestReconcilerHolderForwardsOnceTheCollectorExists closes the other half:
// the holder must be a pass-through, not a swallow-all, or demand-driven
// watches would stop converging for the life of the process.
func TestReconcilerHolderForwardsOnceTheCollectorExists(t *testing.T) {
	h := &reconcilerHolder{}
	real := &countingReconciler{err: errors.New("from the collector")}
	_ = real.Start(context.Background())
	if err := h.Set(context.Background(), real); err != nil {
		t.Fatalf("Set: %v", err)
	}

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
	if err := h.Set(context.Background(), nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

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

// TestABindFailureDuringTheBootWaitExitsWithTheRealError: the servers bind
// in milliseconds while the boot wait runs up to ten seconds, so a health,
// metrics, pprof or stream failure lands INSIDE the wait essentially always.
// The errgroup records it and cancels ctx, the wait returns, and the
// collectors are never built -- so the error serve() returns is a wrapped
// context.Canceled, which run() reads through isContextClosed as a clean
// stop and exits 0.
//
// A port already in use would then be a silent restart loop: a pod
// restarting forever, exit code 0, one misleading "agent stopped" line. The
// run group's own error is the diagnosis and must win over the context one.
func TestABindFailureDuringTheBootWaitExitsWithTheRealError(t *testing.T) {
	bindErr := errors.New("health server: listen tcp :8080: bind: address already in use")

	err := unwindBootFailure(
		func() {},
		func() error { return bindErr },
		func() error { return nil },
		context.Canceled, // what startCollectors returns once the group cancelled ctx
		logger.New("boot-test"),
	)
	if !errors.Is(err, bindErr) {
		t.Fatalf("error = %v, want the run group's own %v", err, bindErr)
	}
	if isContextClosed(err) {
		t.Fatal("run() would read this as a clean stop and exit 0, hiding a hard bind failure")
	}
}

// TestABootFailureWithAHealthyRunGroupKeepsItsOwnCause: the other half --
// when the run group has nothing to say (a real SIGTERM in the wait, or a
// genuine buildCollectors error), the cause stays the answer. A context
// error here SHOULD read as a clean stop.
func TestABootFailureWithAHealthyRunGroupKeepsItsOwnCause(t *testing.T) {
	buildErr := errors.New("log collector: checkpoint dir is not writable")

	err := unwindBootFailure(func() {}, func() error { return nil }, func() error { return nil }, buildErr, logger.New("boot-test"))
	if !errors.Is(err, buildErr) {
		t.Fatalf("error = %v, want %v", err, buildErr)
	}

	// A SIGTERM inside the wait: the group returns the same context error and
	// the process must exit 0, not 1.
	err = unwindBootFailure(func() {}, func() error { return context.Canceled }, func() error { return nil }, context.Canceled, logger.New("boot-test"))
	if !isContextClosed(err) {
		t.Fatalf("error = %v, want a clean stop for a SIGTERM during the boot wait", err)
	}
}

// TestABootFailureAlwaysClosesTheQueue: the queue holds a write-ahead log
// with an open file handle; an unwind that skipped it would leave the WAL
// unclosed on every failed boot.
func TestABootFailureAlwaysClosesTheQueue(t *testing.T) {
	for _, groupErr := range []error{nil, errors.New("health server: bind")} {
		closed := false
		_ = unwindBootFailure(
			func() {},
			func() error { return groupErr },
			func() error { closed = true; return nil },
			errors.New("boom"),
			logger.New("boot-test"),
		)
		if !closed {
			t.Fatalf("the queue was not closed when the run group returned %v", groupErr)
		}
	}
}

// TestReconcilerHolderReplaysTheSetItRefused closes the window Ruling A's
// error alone leaves open. Measured in the backend: the periodic watch
// reconciler (internal/clusterwatch/reconciler.go:188-217) skips a session
// whose signature matches what it last SENT -- sent, not applied -- so a set
// this holder refuses is never re-sent on its own. Healing would otherwise
// need the desired set to change (a lease expiring, 90s TTL) or a
// reconnect, which is up to a minute and a half of a watch a user has just
// asked for being dead.
func TestReconcilerHolderReplaysTheSetItRefused(t *testing.T) {
	h := &reconcilerHolder{}
	desired := []k8sresource.Descriptor{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}}

	if err := h.Reconcile(context.Background(), desired); !errors.Is(err, errReconcilerNotReady) {
		t.Fatalf("setup: error = %v, want errReconcilerNotReady", err)
	}

	real := &countingReconciler{}
	_ = real.Start(context.Background())
	if err := h.Set(context.Background(), real); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if real.applied != 1 {
		t.Fatalf("applied reconciles after Set = %d, want 1 (the refused set must be replayed)", real.applied)
	}
	if len(real.last) != 1 || real.last[0].GVR.Resource != "pods" {
		t.Fatalf("the replayed desired set was not the refused one: %v", real.last)
	}
}

// TestReconcilerHolderReplaysOnlyTheLatestRefusedSet: each reconcile is a
// COMPLETE desired set -- "converge on exactly this" -- so replaying an
// older one after a newer would converge backwards. Drop-and-replay-latest,
// never a queue.
func TestReconcilerHolderReplaysOnlyTheLatestRefusedSet(t *testing.T) {
	h := &reconcilerHolder{}
	older := []k8sresource.Descriptor{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}}
	newer := []k8sresource.Descriptor{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "services"}}}
	_ = h.Reconcile(context.Background(), older)
	_ = h.Reconcile(context.Background(), newer)

	real := &countingReconciler{}
	_ = real.Start(context.Background())
	if err := h.Set(context.Background(), real); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if real.applied != 1 {
		t.Fatalf("applied reconciles = %d, want exactly 1 replay", real.applied)
	}
	if len(real.last) != 1 || real.last[0].GVR.Resource != "services" {
		t.Fatalf("replayed %v, want the LATEST refused set", real.last)
	}
}

// TestReconcilerHolderReplaysNothingWhenItRefusedNothing: the common boot --
// no watch config arrived in the window -- must not synthesize a reconcile
// to an empty desired set, which is the message that stops every
// demand-driven informer.
func TestReconcilerHolderReplaysNothingWhenItRefusedNothing(t *testing.T) {
	h := &reconcilerHolder{}
	real := &countingReconciler{}
	if err := h.Set(context.Background(), real); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if real.calls != 0 {
		t.Fatalf("Reconcile calls = %d, want 0: nothing was ever refused", real.calls)
	}
}

// TestReconcilerHolderDropsTheRefusedSetWhenThereIsNoCollector: an agent
// with collect.state disabled resolves to nil. There is nothing that could
// ever apply the refused set, and it must not be held forever.
func TestReconcilerHolderDropsTheRefusedSetWhenThereIsNoCollector(t *testing.T) {
	h := &reconcilerHolder{}
	_ = h.Reconcile(context.Background(), []k8sresource.Descriptor{
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}},
	})

	if err := h.Set(context.Background(), nil); err != nil {
		t.Fatalf("Set(nil) = %v, want nil", err)
	}
	if err := h.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("Reconcile after resolving to none = %v, want nil", err)
	}
}

// TestReconcilerHolderSurfacesAFailedReplay: serve() logs it, so it must not
// be swallowed here.
func TestReconcilerHolderSurfacesAFailedReplay(t *testing.T) {
	h := &reconcilerHolder{}
	_ = h.Reconcile(context.Background(), []k8sresource.Descriptor{
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}},
	})

	boom := errors.New("informer start failed")
	failing := &countingReconciler{err: boom}
	_ = failing.Start(context.Background())
	if err := h.Set(context.Background(), failing); !errors.Is(err, boom) {
		t.Fatalf("Set = %v, want the replayed Reconcile's own error", err)
	}
}

// TestTheReplayReachesAStartedCollector is the test that was missing, and
// its absence is why the replay shipped inert.
//
// *state.Collector refuses every reconcile until Start has set its runCtx
// (internal/collector/state/reconciler.go: "reconcile: collector has not
// been started"). Resolving the holder before that means the replayed set is
// refused AND cleared by Set -- exactly the behaviour the replay was added
// to fix, plus a log line. So the ordering is what this asserts: the
// collector is started first, and only then is the holder resolved.
func TestTheReplayReachesAStartedCollector(t *testing.T) {
	h := &reconcilerHolder{}
	desired := []k8sresource.Descriptor{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}}
	if err := h.Reconcile(context.Background(), desired); !errors.Is(err, errReconcilerNotReady) {
		t.Fatalf("setup: error = %v, want errReconcilerNotReady", err)
	}

	coll := &countingReconciler{}
	err := startCollector(context.Background(), coll, func(ctx context.Context) error {
		return h.Set(ctx, coll)
	}, logger.New("boot-test"))
	if err != nil {
		t.Fatalf("startCollector: %v", err)
	}

	if coll.applied != 1 {
		t.Fatalf("applied reconciles = %d (calls = %d), want 1: the replay was refused by a "+
			"collector that had not been started, and Set has already cleared the pending set",
			coll.applied, coll.calls)
	}
	if len(coll.last) != 1 || coll.last[0].GVR.Resource != "pods" {
		t.Fatalf("the replayed desired set was not the refused one: %v", coll.last)
	}
}

// TestAReconcileArrivingBeforeTheCollectorStartsIsRemembered is the second
// face of the same ordering bug: with the holder resolved too early, a LIVE
// reconcile in the gap got the collector's hard "not started" error from an
// already-resolved holder, so it was neither applied nor remembered.
// Resolving after Start means that reconcile is refused by the HOLDER, which
// remembers it.
func TestAReconcileArrivingBeforeTheCollectorStartsIsRemembered(t *testing.T) {
	h := &reconcilerHolder{}
	coll := &countingReconciler{}
	desired := []k8sresource.Descriptor{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "services"}}}

	// The gateway pushes a watch config while the collector is mid-Start.
	err := startCollector(context.Background(), coll, func(ctx context.Context) error {
		return h.Set(ctx, coll)
	}, logger.New("boot-test"))
	if err != nil {
		t.Fatalf("startCollector: %v", err)
	}
	// ... and one that arrives after resolution is simply forwarded.
	if err := h.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("Reconcile after the collector started = %v, want nil", err)
	}
	if coll.applied != 1 {
		t.Fatalf("applied = %d, want 1", coll.applied)
	}
}

// TestStartCollectorDoesNotResolveAFailedStart: a collector whose Start
// failed cannot reconcile anything, so the holder must stay unresolved --
// and keep refusing, and keep remembering.
func TestStartCollectorDoesNotResolveAFailedStart(t *testing.T) {
	resolved := false
	err := startCollector(context.Background(), failingCollector{}, func(context.Context) error {
		resolved = true
		return nil
	}, logger.New("boot-test"))
	if err == nil {
		t.Fatal("startCollector must return the collector's Start error")
	}
	if resolved {
		t.Fatal("a collector that failed to start must not resolve the reconciler holder")
	}
}

type failingCollector struct{}

func (failingCollector) Name() string               { return "fake-failing-collector" }
func (failingCollector) Stop(context.Context) error { return nil }
func (failingCollector) Start(_ context.Context) error {
	return errors.New("checkpoint dir is not writable")
}

// TestCollectorWiringFindsWhichCollectorCarriesTheReconciler: the INDEX
// matters, not just the interface value -- the holder may only be resolved
// after that specific collector's Start has returned.
func TestCollectorWiringFindsWhichCollectorCarriesTheReconciler(t *testing.T) {
	rec := &countingReconciler{}
	w := collectorWiring([]Collector{plainCollector{}, rec})
	if w.reconcilerIdx != 1 {
		t.Fatalf("reconcilerIdx = %d, want 1", w.reconcilerIdx)
	}
	if w.reconciler == nil {
		t.Fatal("reconciler must be the collector that carries it")
	}

	// An agent with collect.state disabled: no collector carries one, and
	// serve() must resolve the holder to "none" itself, since no Start ever
	// will.
	none := collectorWiring([]Collector{plainCollector{}})
	if none.reconcilerIdx != -1 || none.reconciler != nil {
		t.Fatalf("wiring = %+v, want no reconciler and index -1", none)
	}
}

// TestABootFailureLogsTheCauseItDoesNotReturn: when the run group's error
// wins, the buildCollectors failure is the line an operator needs and must
// not vanish with it.
func TestABootFailureLogsTheCauseItDoesNotReturn(t *testing.T) {
	var out bytes.Buffer
	log := logger.New("boot-test", logger.WithWriter(&out))
	cause := errors.New("log collector: checkpoint dir is not writable")

	err := unwindBootFailure(
		func() {},
		func() error { return errors.New("health server: bind") },
		func() error { return nil },
		cause,
		log,
	)
	if errors.Is(err, cause) {
		t.Fatal("setup: the group error must win")
	}
	if !strings.Contains(out.String(), "checkpoint dir is not writable") {
		t.Fatalf("the cause was discarded; log was:\n%s", out.String())
	}
}

// TestCollectorsCheckerReportsUnhealthyUntilStarted pins m-2: the checker's
// own doc comment says it "reports unhealthy until serve() has started the
// collectors", and serve() sets started only after startCollector has been
// launched for every collector, not merely scheduled. Nothing previously
// asserted the checker's own two states; this does, directly against the
// type, without going through the whole boot sequence.
func TestCollectorsCheckerReportsUnhealthyUntilStarted(t *testing.T) {
	c := &collectorsChecker{}
	if err := c.Check(context.Background()); err == nil {
		t.Fatal("Check() = nil before started is set, want unhealthy")
	}
	c.started.Store(true)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check() = %v after started is set, want healthy", err)
	}
}
