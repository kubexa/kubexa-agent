package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

var levelMu sync.Mutex

func withLevel(t *testing.T, level Level, fn func()) {
	t.Helper()
	levelMu.Lock()
	orig := GetLevel()
	SetLevel(level)
	t.Cleanup(func() {
		SetLevel(orig)
		levelMu.Unlock()
	})
	fn()
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Level
		wantErr bool
	}{
		{"debug", LevelDebug, false},
		{"INFO", LevelInfo, false},
		{"warn", LevelWarn, false},
		{"warning", LevelWarn, false},
		{"error", LevelError, false},
		{"invalid", LevelInfo, true},
	}

	for _, tt := range tests {
		got, err := ParseLevel(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("ParseLevel(%q): expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestLevelString(t *testing.T) {
	t.Parallel()

	if LevelDebug.String() != "debug" {
		t.Fatalf("got %q", LevelDebug.String())
	}
	if Level(99).String() != "unknown" {
		t.Fatalf("got %q", Level(99).String())
	}
}

func TestSetGetLevelAtomic(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		SetLevel(LevelWarn)
		if GetLevel() != LevelWarn {
			t.Fatalf("GetLevel() = %v", GetLevel())
		}
		SetLevel(LevelError)
		if GetLevel() != LevelError {
			t.Fatalf("GetLevel() = %v", GetLevel())
		}
	})
}

func TestNewJSONOutput(t *testing.T) {
	withLevel(t, LevelDebug, func() {
		var buf bytes.Buffer
		log := New("collector",
			WithClusterID("cluster-1"),
			WithAgentID("agent-1"),
			WithWriter(&buf),
		)

		log.Info("started", F("replicas", 3))

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("invalid JSON: %v\nraw: %s", err, buf.String())
		}

		assertField(t, entry, "component", "collector")
		assertField(t, entry, "cluster_id", "cluster-1")
		assertField(t, entry, "agent_id", "agent-1")
		assertField(t, entry, "level", "info")
		assertField(t, entry, "message", "started")
		assertField(t, entry, "replicas", float64(3))
	})
}

func TestNewDevelopmentOutput(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		log := New("stream",
			WithDevelopment(true),
			WithWriter(&buf),
		)

		log.Info("connected")

		out := buf.String()
		if !strings.Contains(out, "connected") {
			t.Fatalf("expected console output with message, got: %q", out)
		}
		if strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Fatalf("expected non-JSON console output, got: %q", out)
		}
	})
}

func TestWithChildField(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		parent := New("watcher", WithWriter(&buf))
		child := parent.With("namespace", "default")
		child.Info("event")

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		assertField(t, entry, "namespace", "default")
	})
}

func TestWithContextTraceFields(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		log := New("stream", WithWriter(&buf))
		ctx := WithTraceID(context.Background(), "trace-abc")
		ctx = WithSpanID(ctx, "span-xyz")
		ctx = WithRequestID(ctx, "req-123")

		log.WithContext(ctx).Info("handled")

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		assertField(t, entry, "trace_id", "trace-abc")
		assertField(t, entry, "span_id", "span-xyz")
		assertField(t, entry, "request_id", "req-123")
	})
}

func TestLogLevelsFiltered(t *testing.T) {
	withLevel(t, LevelWarn, func() {
		var buf bytes.Buffer
		log := New("collector", WithWriter(&buf))
		log.Debug("debug-msg")
		log.Info("info-msg")
		log.Warn("warn-msg")
		log.Error("error-msg")

		out := buf.String()
		if strings.Contains(out, "debug-msg") || strings.Contains(out, "info-msg") {
			t.Fatalf("unexpected low-level logs: %s", out)
		}
		if !strings.Contains(out, "warn-msg") || !strings.Contains(out, "error-msg") {
			t.Fatalf("missing warn/error logs: %s", out)
		}
	})
}

func TestErrWithoutDebugNoStack(t *testing.T) {
	withLevel(t, LevelError, func() {
		var buf bytes.Buffer
		log := New("collector", WithWriter(&buf))
		err := errors.New("boom")
		log.Err(err).Error("failed")

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if entry["error"] != "boom" {
			t.Fatalf("expected error field, got: %v", entry["error"])
		}
		if _, ok := entry["stack"]; ok {
			t.Fatalf("did not expect stack at error level: %v", entry)
		}
	})
}

func TestErrWithDebugIncludesStack(t *testing.T) {
	withLevel(t, LevelDebug, func() {
		var buf bytes.Buffer
		log := New("collector", WithWriter(&buf))
		err := errors.New("boom")
		log.Err(err).Error("failed")

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if entry["error"] != "boom" {
			t.Fatalf("expected error field, got: %v", entry["error"])
		}
		stack, ok := entry["stack"].(string)
		if !ok || stack == "" {
			t.Fatalf("expected stack field at debug level: %v", entry)
		}
	})
}

func TestContextHelpers(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		base := New("agent", WithWriter(&buf))
		ctx := NewContext(context.Background(), base)

		got := FromContext(ctx)
		if got == nil || got.noop {
			t.Fatal("expected real logger from context")
		}

		got.Info("from-context")
		if !strings.Contains(buf.String(), "from-context") {
			t.Fatal("expected log output from context logger")
		}

		before := buf.Len()
		FromContext(context.Background()).Info("discarded")
		if buf.Len() != before {
			t.Fatal("noop logger should not write")
		}

		//nolint:staticcheck // verifies nil context yields noop logger
		noop2 := FromContext(nil)
		if noop2 == nil {
			t.Fatal("expected noop logger, not nil")
		}
	})
}

func TestNoopLoggerFromEmptyContext(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		log := New("c", WithWriter(&buf))
		noop := FromContext(context.Background())
		noop.With("k", "v").Info("silent", F("a", 1))
		_ = log // ensure real logger still works
		if buf.Len() != 0 {
			t.Fatalf("noop logger wrote output: %s", buf.String())
		}
	})
}

func TestRuntimeLevelChange(t *testing.T) {
	withLevel(t, LevelError, func() {
		var buf bytes.Buffer
		log := New("collector", WithWriter(&buf))
		log.Warn("before")

		SetLevel(LevelWarn)
		log.Warn("after")

		out := buf.String()
		if strings.Contains(out, "before") {
			t.Fatalf("warn should be filtered at error level: %s", out)
		}
		if !strings.Contains(out, "after") {
			t.Fatalf("warn should appear after level change: %s", out)
		}
	})
}

func TestConcurrentSetLevel(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				SetLevel(LevelDebug)
			} else {
				SetLevel(LevelInfo)
			}
			_ = GetLevel()
		}(i)
	}
	wg.Wait()
}

func TestLoggerImplementsInterface(t *testing.T) {
	t.Parallel()

	var _ Interface = New("test")
}

func TestDebugLog(t *testing.T) {
	withLevel(t, LevelDebug, func() {
		var buf bytes.Buffer
		log := New("collector", WithWriter(&buf))
		log.Debug("verbose", F("key", "val"))

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		assertField(t, entry, "level", "debug")
		assertField(t, entry, "message", "verbose")
		assertField(t, entry, "key", "val")
	})
}

func TestNewContextNilUsesBackground(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		log := New("agent", WithWriter(&buf))
		//nolint:staticcheck // verifies nil parent context is treated as background
		ctx := NewContext(nil, log)
		FromContext(ctx).Info("ok")
		if !strings.Contains(buf.String(), "ok") {
			t.Fatalf("expected log output, got: %q", buf.String())
		}
	})
}

func TestErrNilReturnsReceiver(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		log := New("c")
		got := log.Err(nil)
		if got != log {
			t.Fatal("Err(nil) should return same logger")
		}
	})
}

func TestWithLevelOption(t *testing.T) {
	withLevel(t, LevelInfo, func() {
		var buf bytes.Buffer
		log := New("c", WithLevel(LevelError), WithWriter(&buf))
		log.Warn("hidden")
		if buf.Len() != 0 {
			t.Fatalf("expected no output with initial error level, got: %s", buf.String())
		}
	})
}

func assertField(t *testing.T, entry map[string]any, key string, want any) {
	t.Helper()
	got, ok := entry[key]
	if !ok {
		t.Fatalf("missing field %q in %v", key, entry)
	}
	if got != want {
		t.Fatalf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
