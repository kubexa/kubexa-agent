package health

import (
	"context"
	"fmt"
)

// StateWatcherReady reports whether the state watcher informer caches are synced.
type StateWatcherReady interface {
	Ready() bool
}

type stateWatcherChecker struct {
	watcher StateWatcherReady
}

// NewStateWatcherChecker returns a HealthChecker for the Kubernetes state watcher.
func NewStateWatcherChecker(watcher StateWatcherReady) HealthChecker {
	return &stateWatcherChecker{watcher: watcher}
}

// Name returns the component identifier.
func (c *stateWatcherChecker) Name() string {
	return "state-watcher"
}

// Check returns nil when the state watcher caches are synced.
func (c *stateWatcherChecker) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.watcher == nil {
		return Unhealthy(fmt.Errorf("state watcher is not configured"))
	}
	if !c.watcher.Ready() {
		return Unhealthy(fmt.Errorf("state watcher caches not synced"))
	}
	return nil
}
