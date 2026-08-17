package stream

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/ingestrules"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// fakeReconciler is a WatchReconciler test double that records every
// Reconcile call so tests can assert call count (once per config update,
// never once per watcher) and the exact desired set passed.
type fakeReconciler struct {
	mu    sync.Mutex
	calls int
	last  []k8sresource.Descriptor
	err   error
}

func (f *fakeReconciler) Reconcile(_ context.Context, desired []k8sresource.Descriptor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = desired
	return f.err
}

func (f *fakeReconciler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeReconciler) lastDesired() []k8sresource.Descriptor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// newConfigTestManager builds the minimal streamManager needed to exercise
// handleGatewayMessage's config case: a logger and a reconciler. It
// deliberately skips the full New() construction path (dialer, queue,
// metrics) since none of that is reachable from this code path.
func newConfigTestManager(t *testing.T, rec WatchReconciler) *streamManager {
	t.Helper()
	// rules is not optional: the manager is the store's only writer and Set
	// panics on a nil receiver, which is the right behaviour -- a silently
	// swallowed rule push would be worse than a crash. New() guarantees one;
	// this literal has to as well.
	return &streamManager{
		log:        logger.New("stream-config-test"),
		reconciler: rec,
		rules:      ingestrules.NewStore(),
	}
}

// resourceRef splits a "group/version/resource" or "version/resource" test
// spec into a ResourceRef, mirroring the wire shape a gateway would send.
func resourceRef(t *testing.T, spec string) *agentv1.ResourceRef {
	t.Helper()
	parts := strings.Split(spec, "/")
	switch len(parts) {
	case 2:
		return &agentv1.ResourceRef{Version: parts[0], Resource: parts[1]}
	case 3:
		return &agentv1.ResourceRef{Group: parts[0], Version: parts[1], Resource: parts[2]}
	default:
		t.Fatalf("bad resource spec %q", spec)
		return nil
	}
}

// configMessage builds a GatewayMessage carrying a ConfigUpdate whose
// snapshot has one WatcherConfig per entry of watcherResources, each
// populated with the given resource specs. configMessage(nil) yields a
// snapshot with zero watchers, exercising the "no watchers" case.
func configMessage(t *testing.T, watcherResources [][]string) *agentv1.GatewayMessage {
	t.Helper()
	var watchers []*agentv1.WatcherConfig
	for i, specs := range watcherResources {
		var refs []*agentv1.ResourceRef
		for _, spec := range specs {
			refs = append(refs, resourceRef(t, spec))
		}
		watchers = append(watchers, &agentv1.WatcherConfig{
			Id:        strings.Join([]string{"watcher", string(rune('a' + i))}, "-"),
			Resources: refs,
		})
	}
	return &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Config{
			Config: &agentv1.ConfigUpdate{
				ConfigVersion: "test-version",
				Config: &agentv1.ConfigSnapshot{
					Watchers: watchers,
				},
			},
		},
	}
}

func gvrKeys(t *testing.T, descs []k8sresource.Descriptor) []string {
	t.Helper()
	keys := make([]string, 0, len(descs))
	for _, d := range descs {
		keys = append(keys, d.GVR.Group+"/"+d.GVR.Version+"/"+d.GVR.Resource)
	}
	return keys
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			return false
		}
	}
	return true
}

// TestConfigUpdateReconcilesUnionOfWatcherResources is the main-path case:
// two watchers, each with resources, must collapse into exactly one
// Reconcile call carrying the union of both. Calling Reconcile per watcher
// would have the second call's convergence stop what the first call just
// started, since Reconcile always converges to exactly the set it is given.
func TestConfigUpdateReconcilesUnionOfWatcherResources(t *testing.T) {
	rec := &fakeReconciler{}
	m := newConfigTestManager(t, rec)

	msg := configMessage(t, [][]string{
		{"v1/pods"},
		{"apps/v1/deployments", "batch/v1/jobs"},
	})
	m.handleGatewayMessage(context.Background(), msg)

	if got := rec.callCount(); got != 1 {
		t.Fatalf("Reconcile called %d times, want 1", got)
	}
	last := rec.lastDesired()
	if len(last) != 3 {
		t.Fatalf("reconciled to %d descriptors, want 3: %v", len(last), gvrKeys(t, last))
	}
	want := []string{"/v1/pods", "apps/v1/deployments", "batch/v1/jobs"}
	if got := gvrKeys(t, last); !containsAll(got, want) {
		t.Fatalf("reconciled GVRs %v, want to contain %v", got, want)
	}
}

// TestEmptyWatcherConfigReconcilesToEmpty: an empty resources list is a real
// instruction — "nobody is watching anything, stop everything" — not a
// malformed message to skip. Ignoring it would leave informers running
// forever after the last viewer left.
//
// The message is ONE watcher carrying no resources, which is exactly what the
// gateway sends: clusterwatch's syncGroup always emits a single WatcherConfig
// and empties its Resources list. It used to be written as zero watchers,
// which no producer has ever sent and which now means something else --
// "this update is not about watchers", the shape a rules-only ConfigUpdate
// takes. See TestRulesOnlyConfigUpdateLeavesWatchesAlone.
func TestEmptyWatcherConfigReconcilesToEmpty(t *testing.T) {
	rec := &fakeReconciler{}
	m := newConfigTestManager(t, rec)

	m.handleGatewayMessage(context.Background(), configMessage(t, [][]string{{}}))

	if rec.calls != 1 {
		t.Fatalf("Reconcile called %d times, want 1", rec.calls)
	}
	if len(rec.last) != 0 {
		t.Fatalf("reconciled to %d descriptors, want 0", len(rec.last))
	}
}

// TestConfigUpdateSkipsUnparsableResourceButKeepsRest ensures one bad entry
// in a WatcherConfig does not drop the rest of the union — it is logged and
// skipped, and every other resource still reaches Reconcile.
func TestConfigUpdateSkipsUnparsableResourceButKeepsRest(t *testing.T) {
	rec := &fakeReconciler{}
	m := newConfigTestManager(t, rec)

	msg := &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Config{
			Config: &agentv1.ConfigUpdate{
				ConfigVersion: "test-version",
				Config: &agentv1.ConfigSnapshot{
					Watchers: []*agentv1.WatcherConfig{
						{
							Id: "watcher-a",
							Resources: []*agentv1.ResourceRef{
								{Version: "v1", Resource: "pods"},
								{Resource: "not-a-valid-standalone-name"},
							},
						},
					},
				},
			},
		},
	}

	m.handleGatewayMessage(context.Background(), msg)

	if rec.calls != 1 {
		t.Fatalf("Reconcile called %d times, want 1", rec.calls)
	}
	if len(rec.last) != 1 {
		t.Fatalf("reconciled to %d descriptors, want 1 (bad entry must be skipped, not fatal): %v", len(rec.last), gvrKeys(t, rec.last))
	}
	if got := gvrKeys(t, rec.last)[0]; got != "/v1/pods" {
		t.Fatalf("reconciled GVR = %q, want \"/v1/pods\"", got)
	}
}

// TestConfigUpdateWithNilReconcilerDoesNotPanic covers agents running with
// state collection disabled (no WatchReconciler wired): a config update must
// be a no-op, not a nil-pointer panic on the recv loop.
func TestConfigUpdateWithNilReconcilerDoesNotPanic(t *testing.T) {
	m := newConfigTestManager(t, nil)
	m.handleGatewayMessage(context.Background(), configMessage(t, [][]string{{"v1/pods"}}))
}

// TestRulesOnlyConfigUpdateLeavesWatchesAlone is the other side of
// ConfigSnapshot's patch semantics. The ingest-rule pusher sends an update
// carrying only ingest_rules; reconciling that would compute an empty desired
// set and tear down every demand-driven informer the cluster is watching.
func TestRulesOnlyConfigUpdateLeavesWatchesAlone(t *testing.T) {
	rec := &fakeReconciler{}
	m := newConfigTestManager(t, rec)

	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Config{Config: &agentv1.ConfigUpdate{
			ConfigVersion: "rules-only",
			Config: &agentv1.ConfigSnapshot{
				IngestRules: &agentv1.IngestRules{MaxLineBytes: 4096},
			},
		}},
	})

	if rec.callCount() != 0 {
		t.Fatalf("a rules-only update must not reconcile watches, called %d times", rec.callCount())
	}
	if got := m.rules.Get().MaxLineBytes; got != 4096 {
		t.Fatalf("the pushed rules were not applied, MaxLineBytes = %d", got)
	}
}

// TestWatchersOnlyConfigUpdateLeavesRulesAlone completes the pair: a
// clusterwatch update carries no ingest_rules, and nil means unchanged. Without
// that rule the two senders would erase each other's configuration, whichever
// arrived last.
func TestWatchersOnlyConfigUpdateLeavesRulesAlone(t *testing.T) {
	rec := &fakeReconciler{}
	m := newConfigTestManager(t, rec)
	m.rules.Set(ingestrules.Rules{MaxLineBytes: 4096, PerStreamRate: 99})

	m.handleGatewayMessage(context.Background(), configMessage(t, [][]string{{"v1/pods"}}))

	if rec.callCount() != 1 {
		t.Fatalf("a watchers update must still reconcile, called %d times", rec.callCount())
	}
	got := m.rules.Get()
	if got.MaxLineBytes != 4096 || got.PerStreamRate != 99 {
		t.Fatalf("a watchers-only update erased the rules: %+v", got)
	}
}

// TestHandshakeWithoutRulesRestoresTheDefaults. Rules survive a reconnect only
// if the new gateway states them. Carrying a previous session's rules into a
// session with a gateway that pushes none would shape traffic against limits
// nobody currently claims.
func TestHandshakeWithoutRulesRestoresTheDefaults(t *testing.T) {
	m := newConfigTestManager(t, &fakeReconciler{})
	m.rules.Set(ingestrules.Rules{MaxLineBytes: 4096, MaxSampleAge: time.Hour})

	m.applyHandshakeConfig(&agentv1.HandshakeResponse{
		Accepted:     true,
		SessionId:    "s1",
		Config:       &agentv1.ConfigSnapshot{},
		DeliveryAcks: true,
	})

	if got, want := m.rules.Get(), ingestrules.Defaults(); got != want {
		t.Fatalf("a handshake with no rules must restore the defaults\n got: %+v\nwant: %+v", got, want)
	}
	if !m.deliveryAcks.Load() {
		t.Fatal("applyHandshakeConfig did not store delivery_acks from a handshake carrying a config snapshot")
	}
}

// TestHandshakeStoresDeliveryAcksEvenWithNilConfig is the common deployment
// case, not an edge case: the gateway sends a nil ingest Config whenever no
// rules have been pushed for a tenant/cluster, which is the documented
// default. delivery_acks is not part of the config snapshot and must be
// captured regardless of whether one is present -- gating it behind
// "Config != nil" would mean a normal deployment never learns the gateway
// supports acks and every item rides to the 10-minute deadline forever.
func TestHandshakeStoresDeliveryAcksEvenWithNilConfig(t *testing.T) {
	m := newConfigTestManager(t, &fakeReconciler{})

	m.applyHandshakeConfig(&agentv1.HandshakeResponse{
		Accepted:     true,
		SessionId:    "s1",
		Config:       nil,
		DeliveryAcks: true,
	})

	if !m.deliveryAcks.Load() {
		t.Fatal("applyHandshakeConfig did not store delivery_acks from a handshake carrying no config snapshot -- the common deployment case")
	}

	// A later reconnect to a gateway that has since lost the capability (or
	// never had it) must overwrite the stored value, not leave the earlier
	// session's true value in place.
	m.applyHandshakeConfig(&agentv1.HandshakeResponse{
		Accepted:     true,
		SessionId:    "s2",
		Config:       nil,
		DeliveryAcks: false,
	})
	if m.deliveryAcks.Load() {
		t.Fatal("applyHandshakeConfig left a stale true after a handshake reporting delivery_acks=false")
	}
}

// TestQueuedMessageAgeFilter checks the rule at the only place it can be
// checked. A record queued during a three-day outage was fresh when collected
// and is stale when sent, so age is a send-time property.
func TestQueuedMessageAgeFilter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := ingestrules.Rules{MaxSampleAge: time.Hour, MaxFutureSkew: 10 * time.Minute}

	cases := []struct {
		name   string
		ts     time.Time
		reason string // "" = keep
	}{
		{"fresh", now.Add(-time.Minute), ""},
		{"at the age boundary", now.Add(-time.Hour), ""},
		{"too old", now.Add(-2 * time.Hour), "too_old"},
		{"slightly ahead", now.Add(time.Minute), ""},
		{"at the skew boundary", now.Add(10 * time.Minute), ""},
		{"too far ahead", now.Add(time.Hour), "future"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := logMessageAt(tc.ts)
			if got := ageDropReason(msg, rules, now); got != tc.reason {
				t.Errorf("want %q, got %q", tc.reason, got)
			}
		})
	}
}

// TestQueuedMessageAgeFilterOffByDefault: age filtering was not happening
// before this feature and must not start on its own when the gateway pushes
// nothing.
func TestQueuedMessageAgeFilterOffByDefault(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	msg := logMessageAt(now.Add(-100 * 24 * time.Hour))
	if got := ageDropReason(msg, ingestrules.Defaults(), now); got != "" {
		t.Fatalf("with no rule pushed nothing may be dropped, got %q", got)
	}
}

// TestAgeFilterJudgesABatchByItsOldestEntry. A batch is dropped whole, so a
// single fresh entry must not carry a week of stale ones past the gate.
func TestAgeFilterJudgesABatchByItsOldestEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := ingestrules.Rules{MaxSampleAge: time.Hour}
	msg := &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Logs{
		Logs: &agentv1.LogBatch{Entries: []*agentv1.LogEntry{
			{Timestamp: now.Add(-time.Minute).UnixNano()},
			{Timestamp: now.Add(-48 * time.Hour).UnixNano()},
		}},
	}}
	if got := ageDropReason(msg, rules, now); got != "too_old" {
		t.Fatalf("want too_old, got %q", got)
	}
}

// TestAgeFilterIgnoresNonLogPayloads: state events and metrics have their own
// freshness semantics and Loki's reject_old_samples does not apply to them.
func TestAgeFilterIgnoresNonLogPayloads(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := ingestrules.Rules{MaxSampleAge: time.Hour}
	msg := &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Heartbeat{
		Heartbeat: &agentv1.Heartbeat{TimestampUnixMs: now.Add(-72 * time.Hour).UnixMilli()},
	}}
	if got := ageDropReason(msg, rules, now); got != "" {
		t.Fatalf("only log batches are age-filtered, got %q", got)
	}
}

func logMessageAt(ts time.Time) *agentv1.AgentMessage {
	return &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Logs{
		Logs: &agentv1.LogBatch{Entries: []*agentv1.LogEntry{{Timestamp: ts.UnixNano()}}},
	}}
}

// TestHealthSnapshotCarriesTheCounters. The heartbeat is the only path these
// numbers take off the agent, and the gateway advances Prometheus counters by
// the DIFFERENCE between consecutive reports -- so a field left at zero here
// does not merely lose a sample, it makes the next delta wrong.
func TestHealthSnapshotCarriesTheCounters(t *testing.T) {
	m := newConfigTestManager(t, &fakeReconciler{})
	m.counters = ingestrules.NewCounters()
	m.counters.IncTruncated()
	m.counters.IncTooOld()
	m.counters.IncTooOld()
	m.counters.IncFuture()
	m.counters.IncRateLimited()

	h := m.healthSnapshot()
	if h.GetTruncatedLines() != 1 || h.GetDroppedTooOld() != 2 ||
		h.GetDroppedFuture() != 1 || h.GetDroppedRateLimited() != 1 {
		t.Fatalf("counters lost on the way to the heartbeat: %+v", h)
	}
}

// TestHealthSnapshotCountersAreCumulative: they must never decrease within one
// process, because the gateway reads a decrease as an agent restart and adds
// the whole reported value.
func TestHealthSnapshotCountersAreCumulative(t *testing.T) {
	m := newConfigTestManager(t, &fakeReconciler{})
	m.counters = ingestrules.NewCounters()

	m.counters.IncTooOld()
	first := m.healthSnapshot().GetDroppedTooOld()
	m.counters.IncTooOld()
	second := m.healthSnapshot().GetDroppedTooOld()

	if second <= first {
		t.Fatalf("counters must accumulate, got %d then %d", first, second)
	}
}

// TestHealthSnapshotWithoutCountersReportsZero: a manager wired without the
// shared counters must report nothing, not panic.
func TestHealthSnapshotWithoutCountersReportsZero(t *testing.T) {
	m := newConfigTestManager(t, &fakeReconciler{})
	h := m.healthSnapshot()
	if h.GetTruncatedLines() != 0 || h.GetDroppedRateLimited() != 0 {
		t.Fatalf("want zeroes, got %+v", h)
	}
}
