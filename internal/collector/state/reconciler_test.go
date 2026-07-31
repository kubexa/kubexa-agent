package state

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

func desc(group, version, resource string) descriptorFor {
	return descriptorFor{GVR: schema.GroupVersionResource{Group: group, Version: version, Resource: resource}}
}

// The reconciler's whole job: converge on the desired set without disturbing
// what is already correct. Restarting an unchanged informer would drop and
// re-send every object it holds, which the pipeline would write as a burst of
// spurious updates.
func TestReconcileStartsMissingStopsUnwantedLeavesRest(t *testing.T) {
	r := newTestReconciler(t)

	if err := r.reconcile(context.Background(), []descriptorFor{desc("", "v1", "configmaps"), desc("batch", "v1", "cronjobs")}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	firstCronjobs := r.handle("batch/v1/cronjobs")

	if err := r.reconcile(context.Background(), []descriptorFor{desc("batch", "v1", "cronjobs"), desc("example.com", "v1", "widgets")}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if r.running("/v1/configmaps") {
		t.Fatal("configmaps still running after being dropped from the desired set")
	}
	if !r.running("example.com/v1/widgets") {
		t.Fatal("widgets not started after being added to the desired set")
	}
	if r.handle("batch/v1/cronjobs") != firstCronjobs {
		t.Fatal("cronjobs informer was restarted despite staying in the desired set")
	}
}

// A CRD deleted between the catalog sweep and the watch request must not take
// the rest of the reconcile down with it.
func TestReconcileSkipsOneFailureAndKeepsGoing(t *testing.T) {
	r := newTestReconciler(t)
	r.failFor("bad.example.com/v1/broken")

	err := r.reconcile(context.Background(), []descriptorFor{
		desc("bad.example.com", "v1", "broken"),
		desc("batch", "v1", "cronjobs"),
	})
	if err != nil {
		t.Fatalf("reconcile returned %v, want the failure absorbed", err)
	}
	if !r.running("batch/v1/cronjobs") {
		t.Fatal("cronjobs not started — one bad GVR aborted the reconcile")
	}
}

// Stopping a watch does not delete anything from the cluster, so nothing may
// emit DELETED for the objects it was holding.
func TestStoppingAnInformerEmitsNoEvents(t *testing.T) {
	r := newTestReconciler(t)
	_ = r.reconcile(context.Background(), []descriptorFor{desc("batch", "v1", "cronjobs")})
	r.drainEvents()

	_ = r.reconcile(context.Background(), nil)
	time.Sleep(50 * time.Millisecond)

	if n := len(r.drainEvents()); n != 0 {
		t.Fatalf("stopping an informer emitted %d events, want 0", n)
	}
}

// An empty desired set is a valid state (nobody is looking at anything) and
// must not be mistaken for "no configuration yet".
func TestReconcileToEmptyStopsEverything(t *testing.T) {
	r := newTestReconciler(t)
	_ = r.reconcile(context.Background(), []descriptorFor{desc("batch", "v1", "cronjobs")})

	if err := r.reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile to empty: %v", err)
	}
	if r.running("batch/v1/cronjobs") {
		t.Fatal("informer still running after reconciling to an empty set")
	}
}

// descriptorFor is a local alias so the test bodies above (the spec) read
// without a package-qualified type name.
type descriptorFor = k8sresource.Descriptor

// newTestReconciler builds a *Collector wired to a fake dynamic client, ready
// to drive reconcile() directly. It registers List kinds for every GVR the
// tests above name (including the CRD ones) so the fake client's object
// tracker can serve List/Watch for them.
func newTestReconciler(t *testing.T) *Collector {
	t.Helper()

	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "configmaps"}:            "ConfigMapList",
		{Group: "batch", Version: "v1", Resource: "cronjobs"}:         "CronJobList",
		{Group: "example.com", Version: "v1", Resource: "widgets"}:    "WidgetList",
		{Group: "bad.example.com", Version: "v1", Resource: "broken"}: "BrokenList",
	}
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	c := &Collector{
		cfg:     Config{ResyncPeriod: 0},
		dynamic: dynClient,
		log:     logger.New("reconciler-test"),
		metrics: m,
		workCh:  make(chan workItem, 100),
	}

	t.Cleanup(func() {
		// Stop everything the test started so no informer goroutine outlives
		// the test (matters for -race, which watches goroutine lifetimes).
		_ = c.reconcile(context.Background(), nil)
	})

	return c
}

// failFor makes the next attempt to start the given GVR (in "group/version/resource"
// form, matching gvrKey) fail, while every other GVR still builds normally.
func (c *Collector) failFor(key string) {
	c.newDemandInformer = func(desc k8sresource.Descriptor) (dynamicinformer.DynamicSharedInformerFactory, cache.SharedIndexInformer, error) {
		if gvrKey(desc.GVR) == key {
			return nil, nil, fmt.Errorf("simulated start failure for %s", key)
		}
		return c.defaultNewDemandInformer(desc)
	}
}

// running reports whether a demand-driven informer for key (in
// "group/version/resource" form) is currently registered.
func (c *Collector) running(key string) bool {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	_, ok := c.watches[key]
	return ok
}

// handle returns the informer instance registered for key, or nil. Two calls
// returning the same (comparable) value is how the tests confirm an informer
// was left running rather than restarted.
func (c *Collector) handle(key string) cache.SharedIndexInformer {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	w, ok := c.watches[key]
	if !ok {
		return nil
	}
	return w.informer
}

// drainEvents empties the work queue and returns whatever was on it, without
// blocking.
func (c *Collector) drainEvents() []workItem {
	var out []workItem
	for {
		select {
		case item := <-c.workCh:
			out = append(out, item)
		default:
			return out
		}
	}
}
