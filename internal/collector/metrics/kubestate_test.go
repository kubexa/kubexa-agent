package metrics

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestProbeKubeStateServiceFindsTheService(t *testing.T) {
	kube := fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-state-metrics", Namespace: "kube-system"},
	})

	found, err := probeKubeStateService(context.Background(), kube, "kube-system", "kube-state-metrics")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !found {
		t.Fatal("found = false for an existing service")
	}
}

func TestProbeKubeStateServiceReportsAbsence(t *testing.T) {
	kube := fake.NewSimpleClientset()

	found, err := probeKubeStateService(context.Background(), kube, "kube-system", "kube-state-metrics")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if found {
		t.Fatal("found = true with no service present")
	}
}

func TestProbeKubeStateServiceDistinguishesAnAPIErrorFromAbsence(t *testing.T) {
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unreachable")
	})

	found, err := probeKubeStateService(context.Background(), kube, "kube-system", "kube-state-metrics")
	if err == nil {
		t.Fatal("probe returned nil error on an API failure")
	}
	// An API failure is not absence. Reporting "not installed" here would tell
	// the operator to install something that is already there.
	if found {
		t.Fatal("found = true on an API failure")
	}
}

func TestAbsentKubeStateMetricsIsReportedAsNotInstalled(t *testing.T) {
	h := newScrapeHealth()
	kube := fake.NewSimpleClientset()

	found, err := probeKubeStateService(context.Background(), kube, "kube-system", "kube-state-metrics")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !found {
		h.MarkNotInstalled("kube_state_metrics")
	}

	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateNotInstalled {
		t.Fatalf("State = %q, want %q -- the screen must say 'not installed', not 'no data'",
			got.State, StateNotInstalled)
	}
}

// TestKubeStateMetricsTransitionsFromNotInstalledToFailingAsTheServiceAppears
// drives the real probeKubeStateService across both of its outcomes and feeds
// each result into ScrapeHealth exactly the way runKubeStateTarget's switch
// does. It does NOT invoke runKubeStateTarget itself: that function is an
// unbounded ticker loop with no return until ctx is cancelled, and the
// package has no dependency-free way to build a running Collector for it
// (New requires a queue and a kube client; see
// TestStopDynamicTargetClearsTheFailureFromHealth's comment for the same
// constraint on stopDynamicTarget). Confirmed by deliberately removing the
// MarkInstalled call from kubestate.go's found branch: this test kept
// passing, because it re-derives the switch's outcome rather than executing
// it. TestMarkNotInstalledThenMarkInstalledThenFailureReportsFailingNotNotInstalled
// in health_test.go is the test that would fail if that call site regressed;
// this one is here to prove probeKubeStateService's own two outcomes are what
// feed it, not a stand-in value.
//
// A reinstalled-but-still-failing kube-state-metrics must read as failing,
// never as not_installed -- that is the opposite of the collapse the product
// forbids, and it is worse, because it tells the operator there is nothing to
// fix.
func TestKubeStateMetricsTransitionsFromNotInstalledToFailingAsTheServiceAppears(t *testing.T) {
	h := newScrapeHealth()

	// The Service is absent.
	kube := fake.NewSimpleClientset()
	found, err := probeKubeStateService(context.Background(), kube, "kube-system", "kube-state-metrics")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !found {
		h.MarkNotInstalled("kube_state_metrics")
	}
	if got := byKind(h.Snapshot())["kube_state_metrics"]; got.State != StateNotInstalled {
		t.Fatalf("State = %q, want %q before the Service exists", got.State, StateNotInstalled)
	}

	// An operator installs kube-state-metrics: the Service object now
	// exists, but the pod is still crash-looping and the first scrape after
	// the probe finds it fails.
	kube = fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-state-metrics", Namespace: "kube-system"},
	})
	found, err = probeKubeStateService(context.Background(), kube, "kube-system", "kube-state-metrics")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if found {
		h.MarkInstalled("kube_state_metrics")
	}
	h.RecordFailure("kube_state_metrics", "kube-state-metrics", errors.New("HTTP 500"))

	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateFailing {
		t.Fatalf("State = %q, want %q -- a reinstalled-but-failing kube-state-metrics must not "+
			"read as absent", got.State, StateFailing)
	}
}
