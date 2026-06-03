package logs

import (
	"testing"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	fallback := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		line    string
		wantMsg string
		wantLvl agentv1.LogLevel
	}{
		{
			name:    "plain text",
			line:    "hello world",
			wantMsg: "hello world",
			wantLvl: agentv1.LogLevel_LOG_LEVEL_UNSPECIFIED,
		},
		{
			name:    "json info",
			line:    `{"level":"info","msg":"started","time":"2024-06-01T12:00:01Z"}`,
			wantMsg: "started",
			wantLvl: agentv1.LogLevel_LOG_LEVEL_INFO,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			todoImplementParseLineTest(t, tt.line, tt.wantMsg, tt.wantLvl, fallback)
		})
	}
}

func TestMapLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want agentv1.LogLevel
	}{
		{"debug", agentv1.LogLevel_LOG_LEVEL_DEBUG},
		{"INFO", agentv1.LogLevel_LOG_LEVEL_INFO},
		{"warn", agentv1.LogLevel_LOG_LEVEL_WARN},
		{"error", agentv1.LogLevel_LOG_LEVEL_ERROR},
		{"fatal", agentv1.LogLevel_LOG_LEVEL_ERROR},
		{"unknown", agentv1.LogLevel_LOG_LEVEL_UNSPECIFIED},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			todoImplementMapLogLevelTest(t, tt.in, tt.want)
		})
	}
}

func TestFilterPodLabels(t *testing.T) {
	t.Parallel()
	todoImplementFilterPodLabelsTest(t)
}

func todoImplementParseLineTest(t *testing.T, line, wantMsg string, wantLvl agentv1.LogLevel, fallback time.Time) {
	t.Helper()
	got := ParseLine(line, fallback)
	if got.Message != wantMsg {
		t.Fatalf("message = %q, want %q", got.Message, wantMsg)
	}
	if got.Level != wantLvl {
		t.Fatalf("level = %v, want %v", got.Level, wantLvl)
	}
}

func todoImplementMapLogLevelTest(t *testing.T, in string, want agentv1.LogLevel) {
	t.Helper()
	if got := mapLogLevel(in); got != want {
		t.Fatalf("mapLogLevel(%q) = %v, want %v", in, got, want)
	}
}

func todoImplementFilterPodLabelsTest(t *testing.T) {
	t.Helper()
	labels := map[string]string{
		"app":               "api",
		"pod-template-hash": "abc",
	}
	got := FilterPodLabels(labels)
	if _, ok := got["pod-template-hash"]; ok {
		t.Fatal("expected pod-template-hash to be filtered")
	}
	if got["app"] != "api" {
		t.Fatalf("app label = %q, want api", got["app"])
	}
}
