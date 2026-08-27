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
