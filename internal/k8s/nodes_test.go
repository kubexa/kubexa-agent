package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

func node(name, internalIP, externalIP string, port int32, ready bool) *corev1.Node {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	addrs := []corev1.NodeAddress{}
	if externalIP != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: externalIP})
	}
	if internalIP != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: internalIP})
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/os": "linux"}},
		Status: corev1.NodeStatus{
			Addresses:       addrs,
			DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: port}},
			Conditions:      []corev1.NodeCondition{{Type: corev1.NodeReady, Status: cond}},
		},
	}
}

func TestNodesReportsInternalAddressAndReadiness(t *testing.T) {
	kube := fake.NewSimpleClientset(
		node("worker-1", "10.0.0.1", "203.0.113.1", 10250, true),
		node("worker-2", "10.0.0.2", "", 10255, false),
	)
	c := &client{kube: kube, log: logger.New("test")}

	got, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Sorted by name so a caller diffing two inventories sees a stable order.
	if got[0].Name != "worker-1" || got[1].Name != "worker-2" {
		t.Fatalf("order = %q, %q", got[0].Name, got[1].Name)
	}
	// The INTERNAL address, never the external one: the kubelet's serving
	// certificate names the internal address, and the external address is
	// routable from outside the cluster, which is not where the agent is.
	if got[0].InternalIP != "10.0.0.1" {
		t.Errorf("InternalIP = %q, want 10.0.0.1", got[0].InternalIP)
	}
	if got[0].KubeletPort != 10250 {
		t.Errorf("KubeletPort = %d, want 10250", got[0].KubeletPort)
	}
	if !got[0].Ready {
		t.Error("worker-1 Ready = false, want true")
	}
	if got[1].Ready {
		t.Error("worker-2 Ready = true, want false")
	}
	if got[0].Labels["kubernetes.io/os"] != "linux" {
		t.Errorf("Labels = %v", got[0].Labels)
	}
}

func TestNodesSkipsNodesWithNoInternalAddress(t *testing.T) {
	kube := fake.NewSimpleClientset(node("ghost", "", "203.0.113.9", 10250, true))
	c := &client{kube: kube, log: logger.New("test")}

	got, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	// A node with no internal address cannot be scraped, and returning it with
	// an empty host would produce a target whose URL is "https://:10250/..."
	// -- one that fails every scrape and reports as a broken target rather
	// than as an absent one.
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0; got %+v", len(got), got)
	}
}
