package stream

import (
	"context"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
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
	return &streamManager{
		log:        logger.New("stream-config-test"),
		reconciler: rec,
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
func TestEmptyWatcherConfigReconcilesToEmpty(t *testing.T) {
	rec := &fakeReconciler{}
	m := newConfigTestManager(t, rec)

	m.handleGatewayMessage(context.Background(), configMessage(t, nil))

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
