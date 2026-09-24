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

// TestApplyHandshakeConfigSettlesOnAHandshakeWithNoConfig pins m-3: the
// early return for hs.GetConfig() == nil (a gateway that manages nothing for
// this cluster, the common case) must still settle the boot wait. That is
// exactly why settleConfig is called via `defer` rather than at the end of
// the function -- a mutation that settles only when a config is present
// would make every such agent pay the full managedConfigBootWait on every
// start, silently, since nothing else ever closes the channel.
func TestApplyHandshakeConfigSettlesOnAHandshakeWithNoConfig(t *testing.T) {
	m := newManagedTestManager(t)
	m.configSettled = make(chan struct{})

	m.applyHandshakeConfig(&agentv1.HandshakeResponse{
		Accepted:  true,
		SessionId: "s1",
	})

	select {
	case <-m.ConfigSettled():
	default:
		t.Fatal("a handshake carrying no config must still settle the boot wait")
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

// newBootedManagedTestManager is newManagedTestManager after boot has
// reported what the collectors were built from: the file config, which is
// what serve() passes when no managed set arrived inside the boot wait. It
// is the starting point for every restart_required assertion -- without a
// collector config there is nothing built to be stale, and the heartbeat
// answers false for any revision at all.
func newBootedManagedTestManager(t *testing.T) *streamManager {
	t.Helper()
	m := newManagedTestManager(t)
	m.SetCollectorConfig(m.cfg)
	return m
}

// TestTheHeartbeatIsBuiltFromASingleSnapshot pins M-A.
//
// The reviewer reproduced the torn read with a test-only hook instrumenting
// production code, which this task's own TDD rule (and the general shape of
// this codebase's tests) rules out here. The race window itself -- a few
// atomic loads and one healthSnapshot call -- is too small to trigger
// reliably from plain goroutine interleaving without that hook, so a racing
// version of this test would either flake or pass by luck on the very code
// it exists to catch; a flaky pin is worse than a structural one.
//
// Structurally instead: buildHeartbeatFrom takes ONE *managedState and every
// field it derives must come from THAT value, never from a fresh read of
// m.managed. m.managed is left holding a DIFFERENT, recognizable state
// throughout (revision 999, a stale error, a stale warning, and a config
// whose buffer.batch_size differs from the snapshot's) -- so any field that
// leaked back to reading m.managed directly, instead of the snapshot
// argument, would show up here as the wrong value.
func TestTheHeartbeatIsBuiltFromASingleSnapshot(t *testing.T) {
	m := newManagedTestManager(t)

	// collect.metrics is EffectRestart (never EffectLive), so a difference
	// here is exactly what should flip RestartRequired -- buffer or query
	// would not, since RestartRequiredBlocks skips EffectLive blocks
	// entirely regardless of their value.
	built := testConfig()
	built.Collect.Metrics.PodInterval = 30 * time.Second
	m.SetCollectorConfig(built)

	stale := testConfig()
	stale.Collect.Metrics.PodInterval = 30 * time.Second // matches built: if this leaked in, RestartRequired would read false
	m.managed.Store(&managedState{cfg: stale, revision: 999, err: "stale-error", warnings: []string{"stale-warning"}})

	snapCfg := testConfig()
	snapCfg.Collect.Metrics.PodInterval = 45 * time.Second // differs from built: the snapshot's own config demands a restart
	snapshot := &managedState{cfg: snapCfg, revision: 5, err: "", warnings: []string{"snapshot-warning"}}

	hb := m.buildHeartbeatFrom(snapshot)

	if hb.GetAppliedConfigRevision() != 5 {
		t.Errorf("AppliedConfigRevision = %d, want 5 from the snapshot (m.managed still holds 999)",
			hb.GetAppliedConfigRevision())
	}
	if hb.GetConfigError() != "" {
		t.Errorf("ConfigError = %q, want empty from the snapshot (m.managed still holds %q)",
			hb.GetConfigError(), "stale-error")
	}
	if got := hb.GetConfigWarnings(); len(got) != 1 || got[0] != "snapshot-warning" {
		t.Errorf("ConfigWarnings = %v, want [snapshot-warning] from the snapshot", got)
	}
	if want := snapCfg.Collect.State.RedactSecrets; hb.GetCollectRedactSecrets() != want {
		t.Errorf("CollectRedactSecrets = %v, want %v from the snapshot's config",
			hb.GetCollectRedactSecrets(), want)
	}
	if !hb.GetRestartRequired() {
		t.Error("RestartRequired = false, want true: the snapshot's own config (pod_interval 45s) " +
			"differs from built (30s) -- a snapshot-derived answer must see that; m.managed's " +
			"stale config (pod_interval 30s, matching built) would wrongly answer false")
	}
}

// TestHeartbeatReportsRestartRequiredForACollectBlock is C1's symptom, at
// the point it becomes visible: the collectors were built before this
// revision arrived, so collect.logs is stored and validated but NOT in
// effect. Reporting it as applied and nothing else is the "control that
// changes nothing, reported as working" this feature exists to prevent.
func TestHeartbeatReportsRestartRequiredForACollectBlock(t *testing.T) {
	m := newBootedManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 11, map[string]string{
		"collect.logs": "enabled: true\ntail_lines: 42\n",
	}))

	hb := m.buildHeartbeat()
	if hb.GetAppliedConfigRevision() != 11 {
		t.Fatalf("AppliedConfigRevision = %d, want 11", hb.GetAppliedConfigRevision())
	}
	if !hb.GetRestartRequired() {
		t.Fatal("RestartRequired = false; collect.logs is baked into the log collector at " +
			"construction, so revision 11 is applied but not in effect")
	}
}

// TestHeartbeatDoesNotReportRestartRequiredForQueryOnly is the other half:
// the query block IS live (the policy holder swaps on every applied set), so
// asking for a restart would be just as untrue in the other direction -- a
// screen stuck on "waiting for agent restart" for a change that is already
// answering requests.
func TestHeartbeatDoesNotReportRestartRequiredForQueryOnly(t *testing.T) {
	m := newBootedManagedTestManager(t)

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 12, map[string]string{
		"query": "enabled: true\nrules:\n  - resources: [pods]\n",
	}))

	hb := m.buildHeartbeat()
	if hb.GetAppliedConfigRevision() != 12 {
		t.Fatalf("AppliedConfigRevision = %d, want 12", hb.GetAppliedConfigRevision())
	}
	if hb.GetRestartRequired() {
		t.Fatal("RestartRequired = true for a query-only revision, which is live")
	}
}

// TestHeartbeatRestartRequiredFollowsTheBlockClassification is the sibling
// of pkg/config's TestEveryManagedBlockIsClassifiedByWhenItTakesEffect: both
// spell the categories out as literals, so a block that MOVES between them
// fails here until the heartbeat's answer moves with it. Deriving the
// expectation from ManagedBlockEffect instead would agree with any
// classification, including a wrong one, which is exactly the shape of test
// that let four inert blocks report as applied.
//
// restart_required is UNCHANGED on the wire by the three-valued
// classification (Task 7e, C-A/M-B): collect.logs and collect.state are
// EffectPartial now, not EffectRestart, but RestartRequiredBlocks still
// counts a partial block whose value differs -- its construction-time half
// really is pending -- so both cases below still want true. What changed is
// only that a screen must stop reading that true as "nothing has taken
// effect yet" for these two; the live half of each is already running the
// pushed value (see managedBlockEffect's comment in pkg/config/blocks.go).
//
// Each case pushes a value that DIFFERS from the file config the collectors
// were built from -- a block pushed with the value already in effect asks
// for no restart, which its own test covers in pkg/config.
func TestHeartbeatRestartRequiredFollowsTheBlockClassification(t *testing.T) {
	cases := []struct {
		key             string
		yaml            string
		wantRestart     bool
		whyNotRestarted string
	}{
		{"collect.logs", "enabled: true\ntail_lines: 42\n", true, ""},
		{"collect.state", "enabled: true\nresync_period: 17m\n", true, ""},
		{"collect.metrics", "enabled: true\npod_interval: 47s\n", true, ""},
		{"query", "enabled: true\nrules:\n  - resources: [pods]\n", false,
			"the query policy holder swaps on every applied set"},
		{"buffer", "batch_size: 250\n", false,
			"the drain loop re-reads the effective batch size on every batch"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			m := newBootedManagedTestManager(t)
			m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 20, map[string]string{
				tc.key: tc.yaml,
			}))

			hb := m.buildHeartbeat()
			if hb.GetConfigError() != "" {
				t.Fatalf("setup: the block was rejected: %s", hb.GetConfigError())
			}
			if hb.GetAppliedConfigRevision() != 20 {
				t.Fatalf("setup: AppliedConfigRevision = %d, want 20", hb.GetAppliedConfigRevision())
			}
			if got := hb.GetRestartRequired(); got != tc.wantRestart {
				t.Fatalf("RestartRequired = %v, want %v for %s (%s)",
					got, tc.wantRestart, tc.key, tc.whyNotRestarted)
			}
		})
	}
}

// TestRestartRequiredIsFalseBeforeTheCollectorsAreBuilt: during the boot
// wait the collectors do not exist yet, so a set applied in that window is
// exactly what they are about to be built FROM. Reporting "restart
// required" there would tell the operator to restart an agent that is
// already about to run their config.
func TestRestartRequiredIsFalseBeforeTheCollectorsAreBuilt(t *testing.T) {
	m := newManagedTestManager(t) // no SetCollectorConfig: boot has not got there

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 3, map[string]string{
		"collect.logs": "enabled: true\ntail_lines: 42\n",
	}))

	if hb := m.buildHeartbeat(); hb.GetRestartRequired() {
		t.Fatal("RestartRequired = true before any collector was built")
	}
}

// TestBufferBatchSizeFollowsAPush is one of the two live reads Ruling C
// names: drainBufferedQueue read m.cfg directly, which EffectiveConfig()'s
// own doc comment forbids for anything a managed block can change. Without
// this the buffer block is stored, validated, reported as applied -- and the
// drain keeps dequeuing the file config's batch size forever.
func TestBufferBatchSizeFollowsAPush(t *testing.T) {
	m := newManagedTestManager(t)
	m.cfg.Buffer.BatchSize = 100

	if got := m.drainBatchSize(); got != 100 {
		t.Fatalf("setup: drainBatchSize = %d, want the file config's 100", got)
	}

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 4, map[string]string{
		"buffer": "batch_size: 250\n",
	}))

	if got := m.drainBatchSize(); got != 250 {
		t.Fatalf("drainBatchSize = %d, want 250 from the effective config", got)
	}
	if m.cfg.Buffer.BatchSize != 100 {
		t.Fatalf("the file config must stay immutable: BatchSize = %d", m.cfg.Buffer.BatchSize)
	}
}

// TestHandshakeCapabilitiesReportWhatTheCollectorsWereBuiltFrom: the
// handshake advertises which collectors this agent runs, and once boot has
// built them from the effective config, the file config is no longer that
// answer. A reconnect that kept advertising logs=true after the platform
// disabled collect.logs at boot would be the same untruth restart_required
// exists to close, one message earlier.
func TestHandshakeCapabilitiesReportWhatTheCollectorsWereBuiltFrom(t *testing.T) {
	m := newManagedTestManager(t)
	m.cfg.Collect.Logs.Enabled = true

	if !m.agentCaps().GetLogs() {
		t.Fatal("setup: caps must follow the file config before boot reports a collector config")
	}

	// Through ApplyBlocks, not `built := *m.cfg`: a shallow struct copy
	// shares every slice with the config this manager treats as immutable,
	// so an edit here touching a rule list would silently mutate m.cfg --
	// corrupting the very invariant the managed-block feature rests on, in
	// the test that exists to defend it.
	built, err := config.ApplyBlocks(m.cfg, map[string][]byte{
		"collect.logs": []byte("enabled: false\n"),
	})
	if err != nil {
		t.Fatalf("ApplyBlocks: %v", err)
	}
	m.SetCollectorConfig(built)

	if m.agentCaps().GetLogs() {
		t.Fatal("caps.Logs = true while the log collector was never built")
	}
}

// TestAnUnchangedManagedSetIsANoOp is task 7d's finding: this deployment's
// stream is cut and re-established roughly every 150 seconds by design, so
// "on every handshake" means the SAME revision and the SAME block bodies
// land on applyManaged several hundred times a day per cluster. Before this
// test, applyManaged had no unchanged check: every one of those replays ran
// ApplyBlocks again and called the hook again, which in production is the
// live query policy swap plus a force=true nudge to the capability sweep --
// a full SelfSubjectAccessReview pass against the customer's API server on
// every reconnect, forever. The hook call count is the only thing this
// package can observe of that cost (the nudge itself lives in cmd/agent's
// onEffectiveConfig closure), so asserting it stays at one across two
// identical handshakes stands in for both.
func TestAnUnchangedManagedSetIsANoOp(t *testing.T) {
	m := newManagedTestManager(t)
	hookCalls := 0
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		hookCalls++
		return nil, nil
	})

	msg := managedBlocksMsg(t, 5, map[string]string{"buffer": "batch_size: 250\n"})
	m.handleGatewayMessage(context.Background(), msg)
	m.handleGatewayMessage(context.Background(), msg)

	if hookCalls != 1 {
		t.Fatalf("hook called %d times, want 1 -- the second identical handshake must be a no-op", hookCalls)
	}
	rev, errStr, _ := m.ManagedState()
	if rev != 5 || errStr != "" {
		t.Fatalf("ManagedState = (%d, %q), want (5, \"\")", rev, errStr)
	}
	if got := m.EffectiveConfig().Buffer.BatchSize; got != 250 {
		t.Fatalf("EffectiveConfig().Buffer.BatchSize = %d, want 250", got)
	}
}

// TestAFailedSetIsRetriedOnTheNextHandshake is the ruling's non-optional
// half: the skip exists to save work on a set that is already running, not
// to paper over a set that never ran. If the identity check fired on a
// failed attempt too, one transient hook error (a live policy compiler
// hiccup, say) would freeze the agent's config_error for the rest of the
// process's life -- every later identical handshake would match the failed
// attempt's identity and skip, and nothing would ever retry it. The skip
// condition's "last outcome succeeded" clause is what prevents that: it
// reads prev.err, which a failed attempt leaves non-empty.
func TestAFailedSetIsRetriedOnTheNextHandshake(t *testing.T) {
	m := newManagedTestManager(t)
	hookCalls := 0
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		hookCalls++
		if hookCalls == 1 {
			return nil, errors.New("live query policy rejected the new config")
		}
		return nil, nil
	})

	msg := managedBlocksMsg(t, 6, map[string]string{"buffer": "batch_size: 250\n"})
	m.handleGatewayMessage(context.Background(), msg)

	if _, errStr, _ := m.ManagedState(); errStr == "" {
		t.Fatalf("setup: first attempt must fail")
	}

	// Same revision, same bodies -- a naive identity check would treat this
	// as unchanged and skip it, leaving config_error stuck forever.
	m.handleGatewayMessage(context.Background(), msg)

	if hookCalls != 2 {
		t.Fatalf("hook called %d times, want 2 -- a failed set must be retried, not skipped", hookCalls)
	}
	rev, errStr, _ := m.ManagedState()
	if rev != 6 || errStr != "" {
		t.Fatalf("ManagedState = (%d, %q), want (6, \"\") after the retry succeeds", rev, errStr)
	}
}

// TestAChangedBodyAtTheSameRevisionStillApplies pins that the revision alone
// is not the identity: a controller that reuses a revision number across two
// different bodies (a bug, but one this agent must not compound) must still
// see its second body applied, not silently discarded because the revision
// matched the first.
func TestAChangedBodyAtTheSameRevisionStillApplies(t *testing.T) {
	m := newManagedTestManager(t)
	hookCalls := 0
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		hookCalls++
		return nil, nil
	})

	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 8, map[string]string{
		"buffer": "batch_size: 250\n",
	}))
	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 8, map[string]string{
		"buffer": "batch_size: 300\n",
	}))

	if hookCalls != 2 {
		t.Fatalf("hook called %d times, want 2 -- a changed body must apply even at the same revision", hookCalls)
	}
	if got := m.EffectiveConfig().Buffer.BatchSize; got != 300 {
		t.Fatalf("EffectiveConfig().Buffer.BatchSize = %d, want 300", got)
	}
}

// TestANewRevisionWithIdenticalBodiesStillApplies is the pre-flight
// correction: identity cannot be the block bodies alone, because the
// heartbeat echoes the REVISION, not a content hash. A controller can bump
// the revision without changing a single byte of the bodies (a metadata-only
// edit upstream, or simply re-numbering an unchanged set) -- and the
// platform's screen needs the heartbeat to say "applied: 7", not a stale
// "applied: 6", once it has sent 7. A body-only identity would skip this and
// leave the agent reporting a revision the platform has already superseded.
func TestANewRevisionWithIdenticalBodiesStillApplies(t *testing.T) {
	m := newManagedTestManager(t)
	hookCalls := 0
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		hookCalls++
		return nil, nil
	})

	body := map[string]string{"buffer": "batch_size: 250\n"}
	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 6, body))
	m.handleGatewayMessage(context.Background(), managedBlocksMsg(t, 7, body))

	if hookCalls != 2 {
		t.Fatalf("hook called %d times, want 2 -- a new revision must apply even with identical bodies", hookCalls)
	}
	rev, errStr, _ := m.ManagedState()
	if rev != 7 || errStr != "" {
		t.Fatalf("ManagedState = (%d, %q), want (7, \"\") -- the heartbeat must echo the new revision", rev, errStr)
	}
}

// TestRevertingToTheLastGoodSetClearsAStuckError guards the skip condition's
// third clause specifically -- "the previous outcome succeeded" -- by
// constructing the one shape where only that clause matters: a failure whose
// carried-forward revision+identity are NOT the zero value, because they
// were copied from a set that actually succeeded first.
//
// This is deliberately not redundant with TestAFailedSetIsRetriedOnTheNextHandshake:
// that test's first-ever attempt fails with prev == nil, so the identity it
// carries forward is "" (the zero value), which can never equal a real
// hash. That test can pass -- and did pass under a mutation that deleted the
// err clause entirely -- purely because the identity comparison already
// fails on its own; the err clause is never on the path that decides the
// outcome. Only a failure that follows a SUCCESS (so revision and identity
// both match a real prior set) puts prev.err on the critical path: without
// it, reverting to the last known-good config after a transient failure
// would be silently skipped, leaving the platform's screen showing a
// config_error for a config the agent is actually running correctly.
func TestRevertingToTheLastGoodSetClearsAStuckError(t *testing.T) {
	m := newManagedTestManager(t)
	hookCalls := 0
	m.SetOnEffectiveConfig(func(*config.Config) ([]string, error) {
		hookCalls++
		if hookCalls == 2 {
			return nil, errors.New("transient rejection of rev6")
		}
		return nil, nil
	})

	goodMsg := managedBlocksMsg(t, 5, map[string]string{"buffer": "batch_size: 250\n"})
	badMsg := managedBlocksMsg(t, 6, map[string]string{"buffer": "batch_size: 300\n"})

	// Step 1: rev 5 / body A succeeds. State now carries revision=5 and
	// body A's identity forward as the LAST GOOD set.
	m.handleGatewayMessage(context.Background(), goodMsg)
	if rev, errStr, _ := m.ManagedState(); rev != 5 || errStr != "" {
		t.Fatalf("setup: ManagedState = (%d, %q), want (5, \"\")", rev, errStr)
	}

	// Step 2: rev 6 / body B fails in the hook. The error path carries
	// revision=5 and body A's identity forward unchanged (per applyManaged's
	// error branches), alongside the new err string.
	m.handleGatewayMessage(context.Background(), badMsg)
	if _, errStr, _ := m.ManagedState(); errStr == "" {
		t.Fatalf("setup: rev 6 must fail")
	}

	// Step 3: the platform reverts to rev 5 / body A -- the exact set
	// stored as "last good". Revision and identity both match the stored
	// state, so ONLY prev.err != "" can stop this from being wrongly
	// skipped as "unchanged".
	m.handleGatewayMessage(context.Background(), goodMsg)

	if hookCalls != 3 {
		t.Fatalf("hook called %d times, want 3 -- reverting to the last good set after a failure must re-apply, not be skipped as unchanged", hookCalls)
	}
	rev, errStr, _ := m.ManagedState()
	if rev != 5 || errStr != "" {
		t.Fatalf("ManagedState = (%d, %q), want (5, \"\") -- the revert must clear the stuck config_error, not leave it stuck forever", rev, errStr)
	}
}
