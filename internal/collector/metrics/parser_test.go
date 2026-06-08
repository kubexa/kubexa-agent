package metrics

import (
	"strings"
	"testing"
)

func TestParsePrometheusText_Gauge(t *testing.T) {
	input := []byte(`
# HELP http_requests_total Total HTTP requests
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 42 1609459200000
`)
	families, err := ParsePrometheusText(input)
	if err != nil {
		t.Fatalf("ParsePrometheusText() error = %v", err)
	}
	if len(families) != 1 {
		t.Fatalf("families len = %d, want 1", len(families))
	}
	fam := families[0]
	if fam.Name != "http_requests_total" {
		t.Errorf("name = %q", fam.Name)
	}
	if fam.Help != "Total HTTP requests" {
		t.Errorf("help = %q", fam.Help)
	}
	if len(fam.Metrics) != 1 {
		t.Fatalf("metrics len = %d, want 1", len(fam.Metrics))
	}
	m := fam.Metrics[0]
	if m.Value != 42 {
		t.Errorf("value = %v, want 42", m.Value)
	}
	if m.Labels["method"] != "GET" || m.Labels["code"] != "200" {
		t.Errorf("labels = %v", m.Labels)
	}
	if m.Timestamp != 1609459200000 {
		t.Errorf("timestamp = %d", m.Timestamp)
	}
}

func TestMetricFilter_AllowDeny(t *testing.T) {
	filter, err := NewMetricFilter([]string{`^http_.*`}, []string{`.*_debug$`})
	if err != nil {
		t.Fatalf("NewMetricFilter() error = %v", err)
	}
	tests := []struct {
		name  string
		allow bool
	}{
		{"http_requests_total", true},
		{"http_debug", false},
		{"process_cpu_seconds_total", false},
	}
	for _, tc := range tests {
		if got := filter.Allows(tc.name); got != tc.allow {
			t.Errorf("Allows(%q) = %v, want %v", tc.name, got, tc.allow)
		}
	}
}

func TestMetricFilter_EmptyAllowlistAllowsAllExceptDeny(t *testing.T) {
	filter, err := NewMetricFilter(nil, []string{`^go_.*`})
	if err != nil {
		t.Fatalf("NewMetricFilter() error = %v", err)
	}
	if !filter.Allows("process_cpu_seconds_total") {
		t.Error("expected allow when not denied")
	}
	if filter.Allows("go_goroutines") {
		t.Error("expected deny for go_*")
	}
}

func TestFamilyJSON(t *testing.T) {
	fam := ParsedFamily{
		Name: "up",
		Help: "Target up",
		Type: 1,
		Metrics: []ParsedMetric{
			{Labels: map[string]string{"job": "app"}, Value: 1},
		},
	}
	jsonStr, err := FamilyJSON(fam)
	if err != nil {
		t.Fatalf("FamilyJSON() error = %v", err)
	}
	if !strings.Contains(jsonStr, `"name":"up"`) {
		t.Errorf("json = %s", jsonStr)
	}
}

func TestMergeLabels(t *testing.T) {
	got := MergeLabels(map[string]string{"a": "1"}, map[string]string{"b": "2", "a": "override"})
	if got["a"] != "override" || got["b"] != "2" {
		t.Errorf("MergeLabels() = %v", got)
	}
}
