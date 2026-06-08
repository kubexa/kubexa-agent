package metrics

import "testing"

func TestMatchesName(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		want     bool
	}{
		{"api-server-1", []string{"api-server-*"}, true},
		{"web-1", []string{"api-server-*"}, false},
		{"web", nil, true},
		{"node-a", []string{"node-a", "node-b"}, true},
	}
	for _, tc := range tests {
		if got := matchesName(tc.name, tc.patterns); got != tc.want {
			t.Errorf("matchesName(%q, %v) = %v, want %v", tc.name, tc.patterns, got, tc.want)
		}
	}
}

func TestRuleNeedsPodAPIFilter(t *testing.T) {
	if ruleNeedsPodAPIFilter(KubeMetricsRule{}) {
		t.Error("empty rule should not need pod API filter")
	}
	if !ruleNeedsPodAPIFilter(KubeMetricsRule{LabelSelector: "app=api"}) {
		t.Error("label selector should need pod API filter")
	}
}
