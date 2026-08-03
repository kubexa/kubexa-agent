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

// TestParseLineStripsTheAPITimestampPrefix locks in the actual producer
// format: the agent's only log source is CoreV1().Pods(ns).GetLogs with
// Timestamps: true, which prefixes each line with "<RFC3339Nano> " and
// nothing else. There is no "stdout"/"stderr" marker to recover here —
// kubelet consumes that field itself when it renders the response and does
// not re-emit it (see log.proto's reserved field 9, "stream").
func TestParseLineStripsTheAPITimestampPrefix(t *testing.T) {
	line := "2026-08-03T10:00:00.123456789Z boom"
	got := ParseLine(line, time.Now())
	if got.Message != "boom" {
		t.Fatalf("message = %q, want boom", got.Message)
	}
	if string(got.Raw) != "boom" {
		t.Fatalf("raw = %q, want boom", got.Raw)
	}
	wantTS := time.Date(2026, 8, 3, 10, 0, 0, 123456789, time.UTC)
	if !got.Timestamp.Equal(wantTS) {
		t.Fatalf("timestamp = %v, want %v", got.Timestamp, wantTS)
	}
}

// raw is what the consumer writes to Loki. With the timestamp prefix the
// Kubernetes pods/log API adds still attached, `| json` at query time sees
// "2026-...Z {" and parses nothing.
func TestParseLineRawIsThePayloadNotThePrefixedLine(t *testing.T) {
	line := `2026-08-03T10:00:00Z {"level":"error","msg":"boom","trace_id":"abc"}`
	got := ParseLine(line, time.Now())
	if string(got.Raw) != `{"level":"error","msg":"boom","trace_id":"abc"}` {
		t.Fatalf("raw = %q, want the JSON payload alone", got.Raw)
	}
	if got.Message != "boom" {
		t.Fatalf("message = %q, want boom", got.Message)
	}
}

func TestParseLineWithoutPrefixKeepsWholeLineAsRaw(t *testing.T) {
	got := ParseLine("plain text line", time.Now())
	if string(got.Raw) != "plain text line" {
		t.Fatalf("raw = %q", got.Raw)
	}
}
