package health

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/internal/stream"
)

const streamDisconnectGrace = 30 * time.Second

const queueDepthUnhealthyRatio = 0.9

// k8sChecker verifies Kubernetes API reachability.
type k8sChecker struct {
	client k8s.Client
}

// NewK8sChecker returns a HealthChecker that probes the Kubernetes API via client.Ready.
func NewK8sChecker(client k8s.Client) HealthChecker {
	return &k8sChecker{client: client}
}

// Name returns the component identifier.
func (c *k8sChecker) Name() string {
	return "k8s"
}

// Check returns nil when the Kubernetes API is reachable.
func (c *k8sChecker) Check(ctx context.Context) error {
	if c == nil || c.client == nil {
		return Unhealthy(fmt.Errorf("k8s client is not configured"))
	}
	if err := c.client.Ready(ctx); err != nil {
		return Unhealthy(fmt.Errorf("kubernetes api: %w", err))
	}
	return nil
}

// queueChecker evaluates queue depth and drop rate.
type queueChecker struct {
	q           queue.Queue
	maxDropRate float64
	capacity    int64

	mu          sync.Mutex
	lastDropped int64
	lastCheck   time.Time
}

// NewQueueChecker returns a HealthChecker that monitors queue saturation and drop rate.
// When q implements queue.CapacityAware, depth is compared against reported capacity;
// otherwise depth-based unhealthy checks are skipped.
func NewQueueChecker(q queue.Queue, maxDropRate float64) HealthChecker {
	c := &queueChecker{
		q:           q,
		maxDropRate: maxDropRate,
		lastCheck:   time.Now(),
	}
	if capQ, ok := q.(queue.CapacityAware); ok {
		c.capacity = capQ.Capacity()
	}
	return c
}

// Name returns the component identifier.
func (c *queueChecker) Name() string {
	return "queue"
}

// Check returns degraded when the drop rate exceeds maxDropRate per minute, or unhealthy
// when depth exceeds 90% of capacity.
func (c *queueChecker) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.q == nil {
		return Unhealthy(fmt.Errorf("queue is not configured"))
	}

	now := time.Now()
	dropped := c.q.DroppedTotal()

	c.mu.Lock()
	elapsed := now.Sub(c.lastCheck)
	prevDropped := c.lastDropped
	c.lastDropped = dropped
	c.lastCheck = now
	c.mu.Unlock()

	if elapsed > 0 && c.maxDropRate > 0 {
		minutes := elapsed.Minutes()
		if minutes > 0 {
			rate := float64(dropped-prevDropped) / minutes
			if rate > c.maxDropRate {
				return Degraded(fmt.Errorf("drop rate %.2f/min exceeds limit %.2f/min", rate, c.maxDropRate))
			}
		}
	}

	if c.capacity > 0 {
		depth := c.q.Depth()
		threshold := int64(float64(c.capacity) * queueDepthUnhealthyRatio)
		if depth > threshold {
			return Unhealthy(fmt.Errorf("queue depth %d exceeds 90%% of capacity %d", depth, c.capacity))
		}
	}

	return nil
}

// streamChecker verifies the gateway stream has been connected recently.
type streamChecker struct {
	mgr stream.Manager

	mu            sync.Mutex
	lastConnected time.Time
}

// NewStreamChecker returns a HealthChecker that marks the stream unhealthy when
// disconnected for more than 30 seconds.
func NewStreamChecker(mgr stream.Manager) HealthChecker {
	return &streamChecker{
		mgr:           mgr,
		lastConnected: time.Now(),
	}
}

// Name returns the component identifier.
func (c *streamChecker) Name() string {
	return "stream"
}

// Check returns unhealthy when Connected() has been false for more than 30 seconds.
func (c *streamChecker) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.mgr == nil {
		return Unhealthy(fmt.Errorf("stream manager is not configured"))
	}

	now := time.Now()
	connected := c.mgr.Connected()

	c.mu.Lock()
	if connected {
		c.lastConnected = now
	}
	disconnectedFor := now.Sub(c.lastConnected)
	c.mu.Unlock()

	if !connected && disconnectedFor > streamDisconnectGrace {
		return Unhealthy(fmt.Errorf("stream disconnected for %s", disconnectedFor.Truncate(time.Second)))
	}
	return nil
}
