package logs

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectorName(t *testing.T) {
	t.Parallel()
	todoImplementCollectorNameTest(t)
}

func TestConfigFromRoot(t *testing.T) {
	t.Parallel()
	todoImplementConfigFromRootTest(t)
}

func TestMatchesPodName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pod      string
		patterns []string
		want     bool
	}{
		{"empty patterns", "api-1", nil, true},
		{"exact", "api-1", []string{"api-1"}, true},
		{"prefix wildcard", "api-server-0", []string{"api-server-*"}, true},
		{"no match", "worker-0", []string{"api-*"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			todoImplementMatchesPodNameTest(t, tt.pod, tt.patterns, tt.want)
		})
	}
}

func TestCollectorStartStop(t *testing.T) {
	t.Parallel()
	todoImplementCollectorStartStopTest(t)
}

func todoImplementCollectorNameTest(t *testing.T) {
	t.Helper()
	c := &Collector{}
	if c.Name() != componentName {
		t.Fatalf("Name() = %q, want %q", c.Name(), componentName)
	}
}

func todoImplementConfigFromRootTest(t *testing.T) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ApplyDefaults()
	if cfg.ResyncPeriod != defaultResyncPeriod {
		t.Fatalf("resync = %v, want %v", cfg.ResyncPeriod, defaultResyncPeriod)
	}
}

func todoImplementMatchesPodNameTest(t *testing.T, pod string, patterns []string, want bool) {
	t.Helper()
	if got := matchesPodName(pod, patterns); got != want {
		t.Fatalf("matchesPodName(%q, %v) = %v, want %v", pod, patterns, got, want)
	}
}

func todoImplementCollectorStartStopTest(t *testing.T) {
	t.Helper()
	_ = context.Background()
	_ = prometheus.NewRegistry()
	// TODO: construct fake k8s client and queue, verify Start/Stop lifecycle.
}
