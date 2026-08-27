package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

func templ() TargetTemplate {
	return TargetTemplate{
		Kind:            "cadvisor",
		NamePrefix:      "cadvisor",
		Scheme:          "https",
		Port:            "10250",
		Path:            "/metrics/cadvisor",
		Interval:        30 * time.Second,
		Timeout:         10 * time.Second,
		Labels:          map[string]string{"scrape_kind": "cadvisor"},
		BearerTokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		MetricAllowlist: []string{"^container_cpu_.*"},
	}
}

func TestTargetsForNodesBuildsOneTargetPerNode(t *testing.T) {
	nodes := []k8s.NodeInfo{
		{Name: "worker-1", InternalIP: "10.0.0.1", KubeletPort: 10250, Ready: true},
		{Name: "worker-2", InternalIP: "10.0.0.2", KubeletPort: 0, Ready: false},
	}

	got := targetsForNodes(templ(), nodes)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "cadvisor/worker-1" {
		t.Errorf("Name = %q, want cadvisor/worker-1", got[0].Name)
	}
	if got[0].URL != "https://10.0.0.1:10250/metrics/cadvisor" {
		t.Errorf("URL = %q", got[0].URL)
	}
	// templ() sets tpl.Port explicitly, so it wins for every node regardless
	// of what the node itself reports (10250 for worker-1, none at all for
	// worker-2). This test proves the explicit-port override, not the
	// node-owns-the-port fallback -- that is covered separately by
	// TestTargetsForNodesUsesTheNodesOwnKubeletPortWhenTheTemplateLeavesPortEmpty.
	if got[1].URL != "https://10.0.0.2:10250/metrics/cadvisor" {
		t.Errorf("URL = %q", got[1].URL)
	}
	// The node name has to reach the samples. Without it every container
	// series from every node is indistinguishable, and a per-node graph is
	// exactly what this target exists to make possible.
	if got[0].Labels["node"] != "worker-1" {
		t.Errorf("Labels = %v, want node=worker-1", got[0].Labels)
	}
	if got[0].Labels["scrape_kind"] != "cadvisor" {
		t.Errorf("Labels = %v, want scrape_kind=cadvisor", got[0].Labels)
	}
	if got[0].BearerTokenPath != templ().BearerTokenPath {
		t.Errorf("BearerTokenPath = %q", got[0].BearerTokenPath)
	}
	if len(got[0].MetricAllowlist) != 1 {
		t.Errorf("MetricAllowlist = %v", got[0].MetricAllowlist)
	}
}

func TestTargetsForNodesDoesNotShareTheTemplateLabelMap(t *testing.T) {
	tpl := templ()
	nodes := []k8s.NodeInfo{
		{Name: "a", InternalIP: "10.0.0.1", KubeletPort: 10250},
		{Name: "b", InternalIP: "10.0.0.2", KubeletPort: 10250},
	}

	got := targetsForNodes(tpl, nodes)

	// Writing node= into a shared map gives every target the LAST node's name
	// and the samples then all claim one node.
	if got[0].Labels["node"] == got[1].Labels["node"] {
		t.Fatalf("both targets carry node=%q", got[0].Labels["node"])
	}
	if _, leaked := tpl.Labels["node"]; leaked {
		t.Fatal("template label map was mutated")
	}
}

func TestTargetsForNodesFloorsAZeroIntervalAndTimeout(t *testing.T) {
	tpl := templ()
	tpl.Interval = 0
	tpl.Timeout = 0
	nodes := []k8s.NodeInfo{
		{Name: "worker-1", InternalIP: "10.0.0.1", KubeletPort: 10250},
	}

	got := targetsForNodes(tpl, nodes)

	// A zero Interval reaches time.NewTicker in runCustomTarget and panics the
	// whole process, taking every other scraper down with it. The floor has
	// to be applied here, at generation time, not left to the caller.
	if got[0].Interval != pkgconfig.DefaultScrapeInterval {
		t.Errorf("Interval = %v, want the default %v", got[0].Interval, pkgconfig.DefaultScrapeInterval)
	}
	if got[0].Timeout != pkgconfig.DefaultScrapeTimeout {
		t.Errorf("Timeout = %v, want the default %v", got[0].Timeout, pkgconfig.DefaultScrapeTimeout)
	}
}

func TestTargetsForNodesUsesTheNodesOwnKubeletPortWhenTheTemplateLeavesPortEmpty(t *testing.T) {
	tpl := templ()
	tpl.Port = ""
	nodes := []k8s.NodeInfo{
		{Name: "worker-1", InternalIP: "10.0.0.1", KubeletPort: 10255},
		{Name: "worker-2", InternalIP: "10.0.0.2", KubeletPort: 0},
	}

	got := targetsForNodes(tpl, nodes)

	// The node reports its own kubelet port; with no template override that
	// port is what the URL is built on.
	if got[0].URL != "https://10.0.0.1:10255/metrics/cadvisor" {
		t.Errorf("URL = %q, want the node's own kubelet port 10255", got[0].URL)
	}
	// The node reports no kubelet port at all; the fallback is
	// defaultKubeletPort, not a zero or empty port in the URL.
	if got[1].URL != "https://10.0.0.2:"+defaultKubeletPort+"/metrics/cadvisor" {
		t.Errorf("URL = %q, want the default kubelet port %s", got[1].URL, defaultKubeletPort)
	}
}

func TestDiffTargetsReportsAddedAndRemoved(t *testing.T) {
	prev := []ScrapeTarget{{Name: "a", URL: "http://a/m"}, {Name: "b", URL: "http://b/m"}}
	next := []ScrapeTarget{{Name: "b", URL: "http://b/m"}, {Name: "c", URL: "http://c/m"}}

	added, removed := diffTargets(prev, next)

	if len(added) != 1 || added[0].Name != "c" {
		t.Errorf("added = %+v, want [c]", added)
	}
	if len(removed) != 1 || removed[0].Name != "a" {
		t.Errorf("removed = %+v, want [a]", removed)
	}
}

func TestDiffTargetsTreatsAChangedURLAsAReplacement(t *testing.T) {
	// A node keeps its name and changes its address -- a re-created VM, a
	// re-issued lease. Matching on name alone would leave the old scraper
	// running against an address that no longer answers, and its failures
	// would be attributed to the live node.
	prev := []ScrapeTarget{{Name: "cadvisor/w1", URL: "https://10.0.0.1:10250/metrics/cadvisor"}}
	next := []ScrapeTarget{{Name: "cadvisor/w1", URL: "https://10.0.0.9:10250/metrics/cadvisor"}}

	added, removed := diffTargets(prev, next)

	if len(added) != 1 || added[0].URL != "https://10.0.0.9:10250/metrics/cadvisor" {
		t.Errorf("added = %+v", added)
	}
	if len(removed) != 1 || removed[0].URL != "https://10.0.0.1:10250/metrics/cadvisor" {
		t.Errorf("removed = %+v", removed)
	}
}

type stubLister struct {
	calls  int
	result [][]k8s.NodeInfo
	err    error
}

func (s *stubLister) Nodes(context.Context) ([]k8s.NodeInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	i := s.calls
	if i >= len(s.result) {
		i = len(s.result) - 1
	}
	s.calls++
	return s.result[i], nil
}

func TestRefreshReportsOnlyTheDelta(t *testing.T) {
	lister := &stubLister{result: [][]k8s.NodeInfo{
		{{Name: "a", InternalIP: "10.0.0.1", KubeletPort: 10250}},
		{
			{Name: "a", InternalIP: "10.0.0.1", KubeletPort: 10250},
			{Name: "b", InternalIP: "10.0.0.2", KubeletPort: 10250},
		},
	}}
	p := newDynamicProvider(lister, DynamicTargetsConfig{Templates: []TargetTemplate{templ()}}, nil)

	added, removed, err := p.refresh(context.Background())
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if len(added) != 1 || len(removed) != 0 {
		t.Fatalf("first refresh added=%d removed=%d, want 1/0", len(added), len(removed))
	}

	added, removed, err = p.refresh(context.Background())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if len(added) != 1 || added[0].Name != "cadvisor/b" {
		t.Errorf("second refresh added = %+v, want [cadvisor/b]", added)
	}
	if len(removed) != 0 {
		t.Errorf("second refresh removed = %+v, want none", removed)
	}
}

func TestRefreshKeepsCurrentTargetsWhenTheListingFails(t *testing.T) {
	lister := &stubLister{result: [][]k8s.NodeInfo{
		{{Name: "a", InternalIP: "10.0.0.1", KubeletPort: 10250}},
	}}
	p := newDynamicProvider(lister, DynamicTargetsConfig{Templates: []TargetTemplate{templ()}}, nil)
	if _, _, err := p.refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	lister.err = errors.New("apiserver unreachable")
	added, removed, err := p.refresh(context.Background())
	if err == nil {
		t.Fatal("refresh returned nil error on a failed listing")
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("failed listing produced a delta: added=%+v removed=%+v", added, removed)
	}
	if len(p.current) != 1 {
		t.Fatalf("current = %d targets, want the previous 1 kept", len(p.current))
	}
}

// TestStopDynamicTargetClearsTheFailureFromHealth pins the wiring, not just
// the ClearTarget method: a dynamic target that failed its last scrape and is
// then torn down (a node drained and deleted) must stop being counted as
// failing. Before this wiring, stopDynamicTarget touched dynamicCancel and
// customFilters but never health, so the target's failing entry outlived the
// target and TargetsFailing over-reported forever.
//
// The package has no exported, dependency-free way to build a running
// Collector (New requires a queue and, for dynamic targets, a kube client),
// so this white-box test builds the minimal Collector struct literal
// stopDynamicTarget actually touches, rather than adding a new production
// constructor seam just to make it reachable.
func TestStopDynamicTargetClearsTheFailureFromHealth(t *testing.T) {
	c := &Collector{
		health:        newScrapeHealth(),
		dynamicCancel: make(map[string]context.CancelFunc),
		customFilters: make(map[string]*MetricFilter),
	}

	target := ScrapeTarget{
		Name:   "cadvisor/node-3",
		URL:    "https://10.0.0.3:10250/metrics/cadvisor",
		Labels: map[string]string{"scrape_kind": "cadvisor"},
	}

	c.health.SetTargetCount("cadvisor", 3)
	c.health.RecordFailure(healthKind(target), targetLabel(target), errors.New("HTTP 500"))

	got := byKind(c.health.Snapshot())["cadvisor"]
	if got.TargetsFailing != 1 {
		t.Fatalf("TargetsFailing = %d, want 1 before node-3 is removed", got.TargetsFailing)
	}

	_, cancel := context.WithCancel(context.Background())
	c.dynamicCancel[targetIdentity(target)] = cancel
	c.customFilters[targetKey(target)] = &MetricFilter{}

	// The same tick's refresh() drops the denominator to 2; that half of the
	// fix (SetTargetCount) already worked before this change.
	c.stopDynamicTarget(target)
	c.health.SetTargetCount("cadvisor", 2)

	got = byKind(c.health.Snapshot())["cadvisor"]
	if got.TargetsFailing != 0 {
		t.Fatalf("TargetsFailing = %d, want 0: node-3 no longer exists", got.TargetsFailing)
	}
	if got.State != StateOK {
		t.Errorf("State = %q, want %q: the two remaining nodes are healthy", got.State, StateOK)
	}
	if _, running := c.dynamicCancel[targetIdentity(target)]; running {
		t.Error("dynamicCancel entry for node-3 was not removed")
	}
}
