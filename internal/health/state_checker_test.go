package health

import (
	"context"
	"testing"
)

type mockStateWatcher struct {
	ready bool
}

func (m *mockStateWatcher) Ready() bool { return m.ready }

func TestStateWatcherChecker(t *testing.T) {
	t.Parallel()

	notReady := NewStateWatcherChecker(&mockStateWatcher{})
	if err := notReady.Check(context.Background()); err == nil {
		t.Fatal("Check() error = nil, want not synced")
	}

	ready := NewStateWatcherChecker(&mockStateWatcher{ready: true})
	if err := ready.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
}
