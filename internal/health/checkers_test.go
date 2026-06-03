package health

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/watch"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/internal/stream"
)

type mockK8sClient struct {
	ready error
}

func (m *mockK8sClient) Pods(string) k8s.PodClient { return nil }

func (m *mockK8sClient) Logs(context.Context, string, string, string, k8s.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockK8sClient) Watch(context.Context, k8s.ResourceKind, string, k8s.WatchOptions) (watch.Interface, error) {
	return nil, nil
}

func (m *mockK8sClient) NodeMetrics(context.Context) ([]k8s.NodeMetric, error) { return nil, nil }

func (m *mockK8sClient) PodMetrics(context.Context, string) ([]k8s.PodMetric, error) {
	return nil, nil
}

func (m *mockK8sClient) NamespaceUID(context.Context, string) (string, error) { return "", nil }

func (m *mockK8sClient) Ready(ctx context.Context) error { return m.ready }

type mockStreamManager struct {
	connected bool
}

func (m *mockStreamManager) Run(context.Context) error { return nil }

func (m *mockStreamManager) Send(context.Context, *agentv1.AgentMessage) error { return nil }

func (m *mockStreamManager) Connected() bool { return m.connected }

func (m *mockStreamManager) SessionID() string { return "" }

func (m *mockStreamManager) IsThrottled() bool { return false }

var _ stream.Manager = (*mockStreamManager)(nil)

func TestK8sChecker(t *testing.T) {
	t.Parallel()

	checker := NewK8sChecker(&mockK8sClient{ready: errors.New("unreachable")})
	if err := checker.Check(context.Background()); err == nil {
		t.Fatal("Check() error = nil, want failure")
	}

	healthy := NewK8sChecker(&mockK8sClient{})
	if err := healthy.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
}

type mockQueue struct {
	depth    int64
	capacity int64
	dropped  int64
}

func (m *mockQueue) Enqueue(context.Context, queue.Item) error { return nil }
func (m *mockQueue) DequeueBatch(context.Context, int) ([]queue.Item, error) {
	return nil, nil
}
func (m *mockQueue) Ack([]string) error  { return nil }
func (m *mockQueue) Nack([]string) error { return nil }
func (m *mockQueue) Depth() int64        { return m.depth }
func (m *mockQueue) DroppedTotal() int64 { return m.dropped }
func (m *mockQueue) Close() error          { return nil }
func (m *mockQueue) Capacity() int64       { return m.capacity }

var _ queue.CapacityAware = (*mockQueue)(nil)

func TestQueueCheckerDepthUnhealthy(t *testing.T) {
	t.Parallel()

	q := &mockQueue{depth: 95, capacity: 100}
	checker := NewQueueChecker(q, 0)
	checkErr := checker.Check(context.Background())
	if checkErr == nil {
		t.Fatal("Check() error = nil, want unhealthy depth")
	}
	if !strings.Contains(checkErr.Error(), "queue depth") {
		t.Fatalf("Check() error = %v, want depth message", checkErr)
	}
}

func TestStreamCheckerDisconnect(t *testing.T) {
	t.Parallel()

	checker := NewStreamChecker(&mockStreamManager{connected: false}).(*streamChecker)
	checker.lastConnected = time.Now().Add(-31 * time.Second)

	if err := checker.Check(context.Background()); err == nil {
		t.Fatal("Check() error = nil, want disconnected unhealthy")
	}
}
