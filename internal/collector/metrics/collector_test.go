package metrics

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
)

// kubeStub answers Clientset() and nothing else. runKubeStateTarget's presence
// probe is the only k8s.Client method the paths under test reach, and
// embedding the interface keeps this stub from having to grow every time
// k8s.Client does.
type kubeStub struct {
	k8s.Client
	cs kubernetes.Interface
}

func (k kubeStub) Clientset() kubernetes.Interface { return k.cs }

// testCollector builds the Collector struct literal these paths actually
// touch. New requires a queue and a live kube client, and the package has no
// dependency-free way to build a running Collector -- the same constraint
// TestStopDynamicTargetClearsTheFailureFromHealth documents.
func testCollector(t *testing.T, cfg Config, lister nodeLister, cs kubernetes.Interface) *Collector {
	t.Helper()
	m, err := newScraperMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newScraperMetrics: %v", err)
	}
	cfg.ApplyDefaults()
	c := &Collector{
		cfg:           cfg,
		writer:        &queueWriter{},
		log:           logger.New("metrics-test"),
		metrics:       m,
		health:        newScrapeHealth(),
		custom:        newCustomScraper(m),
		customFilters: make(map[string]*MetricFilter),
		dynamicCancel: make(map[string]context.CancelFunc),
		rng:           rand.New(rand.NewSource(1)),
	}
	if cs != nil {
		c.kube = kubeStub{cs: cs}
	}
	if len(cfg.DynamicTargets.Templates) > 0 {
		c.dynamic = newDynamicProvider(lister, cfg.DynamicTargets, c.log)
	}
	return c
}

func cadvisorAndKubeStateConfig() Config {
	return Config{
		Enabled: true,
		DynamicTargets: DynamicTargetsConfig{
			Templates:       []TargetTemplate{templ()},
			RefreshInterval: time.Minute,
		},
		KubeState: KubeStateTarget{
			Enabled:       true,
			ProbeService:  true,
			ProbeInterval: time.Minute,
			Target: ScrapeTarget{
				Name:     "kube-state-metrics",
				Kind:     KindKubeState,
				URL:      "http://kube-state-metrics.kube-system.svc:8080/metrics",
				Interval: 30 * time.Second,
				Timeout:  10 * time.Second,
			},
		},
	}
}

// TestStartSeedsEveryConfiguredKind is the whole-branch defect: for both new
// kinds the ONLY thing that created a health row was a SUCCESSFUL discovery --
// SetTargetCount lived in the else arm of the node listing, and
// kube-state-metrics' behind a successful presence probe. An agent whose
// ClusterRole is stricter than the chart's therefore emitted scrape_targets
// with no entry for either, and the wire contract says an empty or absent
// list means "this agent does not report scrape health at all". A totally
// broken cAdvisor integration would render as an old agent.
//
// Seeding must happen before any discovery runs, so this drives Start with an
// already-cancelled context: every goroutine it launches exits at its first
// ctx.Done check without ever reaching a lister or a probe.
func TestStartSeedsEveryConfiguredKindBeforeAnyDiscoveryRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A lister that would fail if anything called it, and no clientset at all:
	// the seeding under test must not depend on either answering.
	c := testCollector(t, cadvisorAndKubeStateConfig(), &stubLister{err: errors.New("403 nodes is forbidden")}, nil)

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := c.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	snap := byKind(c.health.Snapshot())
	for _, kind := range []string{KindCAdvisor, KindKubeState} {
		got, ok := snap[kind]
		if !ok {
			t.Fatalf("no %q entry: an agent that reports scrape health must emit one row per "+
				"configured kind, or the consumer reads the whole list as 'does not report'", kind)
		}
		// A seeded kind is honest about having measured nothing: the wire
		// documents last_success_unix_ms 0 as "never succeeded", which is a
		// different claim from a measured zero.
		if !got.LastSuccess.IsZero() {
			t.Errorf("%s LastSuccess = %v, want zero", kind, got.LastSuccess)
		}
		if got.SamplesLastScrape != 0 {
			t.Errorf("%s SamplesLastScrape = %d, want 0", kind, got.SamplesLastScrape)
		}
	}
}

// A failing node listing must make the cadvisor kind say so. Before this it
// only logged: the kind kept whatever the last successful listing wrote, and
// on the first attempt that is nothing at all -- an absence indistinguishable
// from an agent that does not report.
func TestAFailingNodeListingReportsTheKindAsFailingNotAbsent(t *testing.T) {
	cfg := cadvisorAndKubeStateConfig()
	c := testCollector(t, cfg, &stubLister{err: errors.New("nodes is forbidden: RBAC")}, nil)
	c.health.SetTargetCount(KindCAdvisor, 0) // what Start seeds

	// An already-cancelled context runs exactly one refresh and returns:
	// runDynamicTargets refreshes first and only then selects on ctx.Done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.runDynamicTargets(ctx)

	got, ok := byKind(c.health.Snapshot())[KindCAdvisor]
	if !ok {
		t.Fatal("no cadvisor entry after a failed node listing")
	}
	if got.State != StateFailing {
		t.Fatalf("State = %q, want %q -- a discovery the agent cannot perform is a failure, "+
			"not an absence and not an OK", got.State, StateFailing)
	}
}

// And a listing that starts working again must stop reporting failing: no
// scrape is ever attributed to the synthetic discovery entry, so nothing else
// would ever clear it.
func TestARecoveredNodeListingClearsTheDiscoveryFailure(t *testing.T) {
	cfg := cadvisorAndKubeStateConfig()
	lister := &stubLister{err: errors.New("apiserver unreachable")}
	c := testCollector(t, cfg, lister, nil)
	c.health.SetTargetCount(KindCAdvisor, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.runDynamicTargets(ctx)
	if got := byKind(c.health.Snapshot())[KindCAdvisor]; got.State != StateFailing {
		t.Fatalf("State = %q, want %q while the listing fails", got.State, StateFailing)
	}

	lister.err = nil
	lister.result = [][]k8s.NodeInfo{{{Name: "a", InternalIP: "10.0.0.1", KubeletPort: 10250}}}
	c.runDynamicTargets(ctx)

	got := byKind(c.health.Snapshot())[KindCAdvisor]
	if got.State != StateOK {
		t.Fatalf("State = %q, want %q once the listing recovered", got.State, StateOK)
	}
}

// The kube-state-metrics half of the same defect. A probe that cannot ask --
// 403 on `get services` -- previously only logged, so the kind never appeared
// at all. It must read failing: not_installed would be a claim the agent has
// no evidence for, and absence would blind the operator whose RBAC is wrong.
func TestAFailingPresenceProbeReportsFailingNotAbsentAndNotNotInstalled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("services is forbidden: RBAC")
	})

	cfg := cadvisorAndKubeStateConfig()
	cfg.DynamicTargets = DynamicTargetsConfig{}
	cfg.KubeState.Target.Interval = 20 * time.Millisecond
	cfg.KubeState.Target.Timeout = 5 * time.Millisecond
	c := testCollector(t, cfg, nil, cs)
	c.health.SetTargetCount(KindKubeState, 0) // what Start seeds

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runKubeStateTarget(ctx)
	}()

	waitFor(t, func() bool {
		return byKind(c.health.Snapshot())[KindKubeState].State == StateFailing
	}, "kube_state_metrics never reported failing after a refused presence probe")
	cancel()
	<-done

	got := byKind(c.health.Snapshot())[KindKubeState]
	if got.State == StateNotInstalled {
		t.Fatal("State = not_installed on a probe that could not ask; the agent has no evidence of absence")
	}
	if got.State != StateFailing {
		t.Fatalf("State = %q, want %q", got.State, StateFailing)
	}
}

// The probe answering is what clears the "could not ask" entry, whichever way
// it answers. A found Service that then fails its scrape must read failing on
// the scrape's own evidence, not on a stale discovery entry.
func TestAProbeThatAnswersClearsTheDiscoveryFailure(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-state-metrics", Namespace: "kube-system"},
	})

	cfg := cadvisorAndKubeStateConfig()
	cfg.DynamicTargets = DynamicTargetsConfig{}
	cfg.KubeState.ServiceNamespace = "kube-system"
	cfg.KubeState.ServiceName = "kube-state-metrics"
	cfg.KubeState.Target.Interval = 20 * time.Millisecond
	cfg.KubeState.Target.Timeout = 5 * time.Millisecond
	c := testCollector(t, cfg, nil, cs)
	c.health.SetTargetCount(KindKubeState, 0)
	c.health.RecordDiscoveryFailure(KindKubeState)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runKubeStateTarget(ctx)
	}()

	waitFor(t, func() bool {
		return byKind(c.health.Snapshot())[KindKubeState].TargetsTotal == 1
	}, "the presence probe never found the Service")
	cancel()
	<-done

	// The scrape against a URL nothing serves fails, and that failure is the
	// target's own -- one entry, not two. A lingering discovery entry would
	// double-count it and report 2 of 1 failing.
	got := byKind(c.health.Snapshot())[KindKubeState]
	if got.TargetsFailing > got.TargetsTotal {
		t.Fatalf("targets = %d/%d -- the discovery entry outlived the probe that answered",
			got.TargetsFailing, got.TargetsTotal)
	}
}

// stopAllDynamicTargets used to leave p.current populated and every target's
// failing entry standing, so a frozen registry reported a fleet nobody scrapes.
func TestStopAllDynamicTargetsClearsTheProvidersView(t *testing.T) {
	cfg := cadvisorAndKubeStateConfig()
	lister := &stubLister{result: [][]k8s.NodeInfo{{
		{Name: "a", InternalIP: "10.0.0.1", KubeletPort: 10250},
		{Name: "b", InternalIP: "10.0.0.2", KubeletPort: 10250},
	}}}
	c := testCollector(t, cfg, lister, nil)

	if _, _, err := c.dynamic.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	c.health.SetTargetCount(KindCAdvisor, 2)
	c.health.RecordFailure(KindCAdvisor, "cadvisor/a", errors.New("HTTP 500"))

	c.stopAllDynamicTargets()

	if len(c.dynamic.current) != 0 {
		t.Errorf("current = %d targets after a stop, want none", len(c.dynamic.current))
	}
	if got := byKind(c.health.Snapshot())[KindCAdvisor]; got.TargetsFailing != 0 {
		t.Errorf("TargetsFailing = %d after a stop, want 0: nothing is being scraped",
			got.TargetsFailing)
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
