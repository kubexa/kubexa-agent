package stream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/kubexa/kubexa-agent/internal/ingestrules"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
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

// newManagedTestManager builds a streamManager with a real, valid file
// config -- unlike newConfigTestManager, applyManaged needs one that
// config.ApplyBlocks can actually overlay and validate. It skips New() for
// the same reason newConfigTestManager does: none of dialing, the queue or
// metrics is reachable from applyManaged, handleGatewayMessage's config case
// or applyHandshakeConfig.
func newManagedTestManager(t *testing.T) *streamManager {
	t.Helper()
	return &streamManager{
		cfg:   testConfig(),
		log:   logger.New("stream-managed-test"),
		rules: ingestrules.NewStore(),
	}
}

// managedBlocksMsg builds a GatewayMessage carrying a ConfigUpdate whose
// snapshot's managed field is nil when blocks is nil (the sender said
// nothing about managed blocks -- ConfigSnapshot's patch rule), or a
// ManagedBlocks set (possibly with zero entries, for a released block) when
// blocks is non-nil. revision is carried on the set itself, not on
// ConfigUpdate.config_version -- that field belongs to other senders (the
// ingest-rules pusher) and is absent from the handshake's ConfigSnapshot
// entirely, so it plays no part in the managed-block revision.
func managedBlocksMsg(t *testing.T, revision int64, blocks map[string]string) *agentv1.GatewayMessage {
	t.Helper()
	var set *agentv1.ManagedBlocks
	if blocks != nil {
		mb := &agentv1.ManagedBlocks{Revision: revision}
		for key, yamlBody := range blocks {
			mb.Blocks = append(mb.Blocks, &agentv1.ManagedBlock{Key: key, Yaml: yamlBody})
		}
		set = mb
	}
	return &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Config{
			Config: &agentv1.ConfigUpdate{
				Config: &agentv1.ConfigSnapshot{Managed: set},
			},
		},
	}
}

// TestManagedBlocksApplyFromTheHandshake: a managed block in the handshake's
// config snapshot must land in EffectiveConfig, and must never touch the
// immutable file config it was overlaid on.
func TestManagedBlocksApplyFromTheHandshake(t *testing.T) {
	m := newManagedTestManager(t)
	fileBatchSize := m.cfg.Buffer.BatchSize

	m.applyHandshakeConfig(&agentv1.HandshakeResponse{
		Accepted:  true,
		SessionId: "s1",
		Config: &agentv1.ConfigSnapshot{
			Managed: &agentv1.ManagedBlocks{
				Revision: 4,
				Blocks:   []*agentv1.ManagedBlock{{Key: "buffer", Yaml: "batch_size: 250\n"}},
			},
		},
	})

	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("EffectiveConfig().Buffer.BatchSize = %d, want 250", got)
	}
	if m.cfg.Buffer.BatchSize != fileBatchSize {
		t.Fatalf("the file config must not be mutated: BatchSize = %d, want %d", m.cfg.Buffer.BatchSize, fileBatchSize)
	}
	// The handshake carries no config_version of its own -- the revision
	// must come from the set itself, or a handshake-only agent could never
	// report one at all.
	if rev, _, _ := m.ManagedState(); rev != 4 {
		t.Fatalf("ManagedState revision = %d, want 4", rev)
	}
}

// TestManagedBlocksApplyFromAConfigUpdate is the handshake test's sibling on
// the push path: the same overlay through GatewayMessage_Config, and the
// config_version on the wire must show up as ManagedState's revision.
func TestManagedBlocksApplyFromAConfigUpdate(t *testing.T) {
	m := newManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 7, map[string]string{
		"buffer": "batch_size: 300\n",
	}))

	if got := m.EffectiveConfig().Buffer.BatchSize; got != 300 {
		t.Fatalf("EffectiveConfig().Buffer.BatchSize = %d, want 300", got)
	}
	rev, errStr, _ := m.ManagedState()
	if rev != 7 {
		t.Fatalf("ManagedState revision = %d, want 7", rev)
	}
	if errStr != "" {
		t.Fatalf("ManagedState err = %q, want empty", errStr)
	}
}

// TestAWatchersOnlyUpdateKeepsTheManagedBlocks pins ConfigSnapshot's patch
// rule on the managed field specifically: a nil managed field means the
// sender said nothing about managed blocks, not "clear them". A plausible
// wrong implementation would treat any non-managed update as a reset.
func TestAWatchersOnlyUpdateKeepsTheManagedBlocks(t *testing.T) {
	m := newManagedTestManager(t)
	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 5, map[string]string{
		"buffer": "batch_size: 250\n",
	}))

	// A watchers-only update, like clusterwatch's reconciler sends, carries
	// a nil managed field.
	m.handleGatewayMessage(context.Background(), configMessage(t, [][]string{{"v1/pods"}}))

	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("a watchers-only update erased the managed blocks: BatchSize = %d, want 250", got)
	}
	if rev, _, _ := m.ManagedState(); rev != 5 {
		t.Fatalf("ManagedState revision = %d, want 5 (unchanged)", rev)
	}
}

// TestAReleasedBlockRestoresTheFileValue: a non-nil managed set that simply
// omits a previously-set key means the platform released that block. Since
// ApplyBlocks always overlays onto a fresh copy of the immutable file
// config, the file's own value comes back rather than the last value the
// platform pushed.
func TestAReleasedBlockRestoresTheFileValue(t *testing.T) {
	m := newManagedTestManager(t)
	fileBatchSize := m.cfg.Buffer.BatchSize

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 1, map[string]string{
		"buffer": "batch_size: 250\n",
	}))
	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("setup: BatchSize = %d, want 250", got)
	}

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 2, map[string]string{}))

	if got := m.EffectiveConfig().Buffer.BatchSize; got != fileBatchSize {
		t.Fatalf("a released block did not restore the file value: BatchSize = %d, want %d", got, fileBatchSize)
	}
	if rev, _, _ := m.ManagedState(); rev != 2 {
		t.Fatalf("ManagedState revision = %d, want 2", rev)
	}
}

// TestABadBlockAppliesNothingAndIsReported: a block ApplyBlocks refuses must
// leave EffectiveConfig exactly where it was -- all-or-nothing, never a
// partial apply -- and ManagedState's error must name the offending block
// rather than being applied silently or crashing the agent.
func TestABadBlockAppliesNothingAndIsReported(t *testing.T) {
	m := newManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 3, map[string]string{
		"buffer": "batch_size: -1\n",
	}))

	if got, want := m.EffectiveConfig().Buffer.BatchSize, m.cfg.Buffer.BatchSize; got != want {
		t.Fatalf("a rejected block must leave the effective config unchanged: BatchSize = %d, want %d", got, want)
	}
	rev, errStr, _ := m.ManagedState()
	if rev != 0 {
		t.Fatalf("a rejected revision must not be reported as applied, got %d", rev)
	}
	if !strings.Contains(errStr, "buffer") {
		t.Fatalf("ManagedState err must name the block, got %q", errStr)
	}
}

// TestManagedRevisionSurvivesAReconnectThroughTheHandshake reproduces the
// review finding: a block applied through a numbered ConfigUpdate, then
// re-applied through a handshake (an agent restart or reconnect re-adopting
// the same platform-owned set), must still report the ORIGINAL revision --
// not 0, which the platform's screen cannot tell apart from "nothing has
// ever been applied". This is the save-then-restart flow the whole feature
// turns on.
func TestManagedRevisionSurvivesAReconnectThroughTheHandshake(t *testing.T) {
	m := newManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 12, map[string]string{
		"buffer": "batch_size: 250\n",
	}))
	if rev, _, _ := m.ManagedState(); rev != 12 {
		t.Fatalf("setup: ManagedState revision = %d, want 12", rev)
	}

	// The agent restarts (or reconnects) and picks the same platform-owned
	// set back up from the handshake's config snapshot, revision included.
	m.applyHandshakeConfig(&agentv1.HandshakeResponse{
		Accepted:  true,
		SessionId: "s2",
		Config: &agentv1.ConfigSnapshot{
			Managed: &agentv1.ManagedBlocks{
				Revision: 12,
				Blocks:   []*agentv1.ManagedBlock{{Key: "buffer", Yaml: "batch_size: 250\n"}},
			},
		},
	})

	if rev, _, _ := m.ManagedState(); rev != 12 {
		t.Fatalf("revision did not survive the handshake: ManagedState revision = %d, want 12", rev)
	}
	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("EffectiveConfig().Buffer.BatchSize = %d, want 250", got)
	}
}

// TestHeartbeatReportsTheAppliedRevision: a successfully applied managed
// block set must show up on the heartbeat payload with no config_error.
func TestHeartbeatReportsTheAppliedRevision(t *testing.T) {
	m := newManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 9, map[string]string{
		"buffer": "batch_size: 250\n",
	}))

	hb := m.buildHeartbeat()
	if hb.GetAppliedConfigRevision() != 9 {
		t.Fatalf("AppliedConfigRevision = %d, want 9", hb.GetAppliedConfigRevision())
	}
	if hb.GetConfigError() != "" {
		t.Fatalf("ConfigError = %q, want empty", hb.GetConfigError())
	}
}

// TestHeartbeatReportsARejectedBlock: the revision must stay at the last
// APPLIED one and the error must be set. A rejected revision reported as
// applied makes the screen say "applied" for a config the agent refused to
// run.
func TestHeartbeatReportsARejectedBlock(t *testing.T) {
	m := newManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 5, map[string]string{
		"buffer": "batch_size: 250\n",
	}))
	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 6, map[string]string{
		"buffer": "batch_size: -1\n",
	}))

	hb := m.buildHeartbeat()
	if hb.GetAppliedConfigRevision() != 5 {
		t.Fatalf("AppliedConfigRevision = %d, want 5 (the rejected rev 6 must not advance it)", hb.GetAppliedConfigRevision())
	}
	if !strings.Contains(hb.GetConfigError(), "buffer") {
		t.Fatalf("ConfigError must name the offending block, got %q", hb.GetConfigError())
	}
}

// TestHeartbeatReportsBothRedactionFlags: both flags come from the EFFECTIVE
// config, not the immutable file config -- proven here by diverging the two,
// storing an overlay directly (as applyManaged would after a managed block),
// and confirming buildHeartbeat follows the overlay. Both flags are ALWAYS
// set (proto.Bool), never left nil, because this agent always knows them.
func TestHeartbeatReportsBothRedactionFlags(t *testing.T) {
	m := newManagedTestManager(t)
	m.cfg.Collect.State.RedactSecrets = false
	m.cfg.Query.RedactSecrets = nil

	effective := *m.cfg
	effective.Collect.State.RedactSecrets = true
	// The file config's Query.RedactSecrets is nil, so its QueryRedactSecrets()
	// falls back to Collect.State.RedactSecrets = false. Setting the
	// effective config's override to true, not false, makes the two configs
	// disagree on this flag too -- a false override would equal the file
	// config's fallback value and the assertion below would pass even if
	// buildHeartbeat read the wrong config.
	effective.Query.RedactSecrets = proto.Bool(true)
	m.managed.Store(&managedState{cfg: &effective, revision: 1})

	hb := m.buildHeartbeat()
	if hb.CollectRedactSecrets == nil || !hb.GetCollectRedactSecrets() {
		t.Fatalf("CollectRedactSecrets = %v, want true (from EffectiveConfig, not the file config)", hb.GetCollectRedactSecrets())
	}
	if hb.QueryRedactSecrets == nil || !hb.GetQueryRedactSecrets() {
		t.Fatalf("QueryRedactSecrets = %v, want true (the effective config's explicit override)", hb.GetQueryRedactSecrets())
	}
}

// TestHeartbeatCarriesTheWarningsTheCallbackReturned: Ruling A -- the
// unredacted-wildcard warning the onEffectiveConfig callback returns must
// reach the heartbeat, not just the agent's own log, so the person who
// pushed the config from the platform can see it too.
func TestHeartbeatCarriesTheWarningsTheCallbackReturned(t *testing.T) {
	m := newManagedTestManager(t)
	wantWarnings := []string{"query allows every resource and redaction is off"}
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		return wantWarnings, nil
	})

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 3, map[string]string{
		"buffer": "batch_size: 250\n",
	}))

	hb := m.buildHeartbeat()
	if len(hb.GetConfigWarnings()) != 1 || hb.GetConfigWarnings()[0] != wantWarnings[0] {
		t.Fatalf("ConfigWarnings = %v, want %v", hb.GetConfigWarnings(), wantWarnings)
	}
}

// TestSetOnEffectiveConfigNilLeavesTheSwapUnwired: SetOnEffectiveConfig's own
// doc comment says fn may be nil, "the same as never calling this at all" --
// pin that a nil registration doesn't panic applyManaged now that the field
// is an atomic.Pointer (a stored pointer to a nil func is itself non-nil).
func TestSetOnEffectiveConfigNilLeavesTheSwapUnwired(t *testing.T) {
	m := newManagedTestManager(t)
	m.SetOnEffectiveConfig(nil)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 1, map[string]string{
		"buffer": "batch_size: 250\n",
	}))

	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("EffectiveConfig().Buffer.BatchSize = %d, want 250", got)
	}
	if rev, errStr, _ := m.ManagedState(); rev != 1 || errStr != "" {
		t.Fatalf("ManagedState = (%d, %q), want (1, \"\")", rev, errStr)
	}
}

// TestAFailedSwapDoesNotAdvanceTheAppliedRevision is Ruling B, the one that
// matters most: ApplyBlocks accepting a value its real consumer (the query
// policy compiler, here faked) refuses is a mirrored-constraint gap, and
// reporting that set as applied would be worse than refusing it. Revision 12
// applies cleanly; revision 13 is valid AS FAR AS ApplyBlocks is concerned,
// but the callback -- standing in for a live policy swap that fails -- must
// reject the whole set, leaving 12 as the last APPLIED revision everywhere:
// the heartbeat, config_error, and EffectiveConfig() itself.
func TestAFailedSwapDoesNotAdvanceTheAppliedRevision(t *testing.T) {
	m := newManagedTestManager(t)
	calls := 0
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return nil, errors.New("query policy rejected the new config")
	})

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 12, map[string]string{
		"buffer": "batch_size: 250\n",
	}))
	if rev, errStr, _ := m.ManagedState(); rev != 12 || errStr != "" {
		t.Fatalf("setup: ManagedState = (%d, %q), want (12, \"\")", rev, errStr)
	}
	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("setup: EffectiveConfig().Buffer.BatchSize = %d, want 250", got)
	}

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 13, map[string]string{
		"buffer": "batch_size: 300\n",
	}))

	rev, errStr, _ := m.ManagedState()
	if rev != 12 {
		t.Fatalf("a failed swap must not advance the applied revision: ManagedState revision = %d, want 12", rev)
	}
	if errStr == "" {
		t.Fatalf("ManagedState err must be set when the swap rejects the config")
	}
	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("EffectiveConfig() must still be revision 12's config: BatchSize = %d, want 250 (unchanged)", got)
	}

	hb := m.buildHeartbeat()
	if hb.GetAppliedConfigRevision() != 12 {
		t.Fatalf("heartbeat AppliedConfigRevision = %d, want 12", hb.GetAppliedConfigRevision())
	}
	if hb.GetConfigError() == "" {
		t.Fatalf("heartbeat ConfigError must be set after a rejected swap")
	}
}
