package pprof

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

func TestNewServerReturnsNilWhenAddrEmpty(t *testing.T) {
	// Empty addr is the default and means OFF. A heap dump from this agent
	// carries unredacted secret values and customer log lines, so "off" has to
	// be what an operator gets by doing nothing.
	if srv := NewServer("", nil); srv != nil {
		t.Fatalf("NewServer(\"\") = %v, want nil", srv)
	}
}

func TestRunOnNilServerIsANoOp(t *testing.T) {
	var srv *Server
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Run(ctx); err != nil {
		t.Fatalf("Run on nil server: %v", err)
	}
}

func TestServesProfilesOnItsOwnAddr(t *testing.T) {
	addr := freeLocalAddr(t)
	srv := NewServer(addr, logger.New("pprof-test"))
	if srv == nil {
		t.Fatal("NewServer returned nil for a non-empty addr")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitForListener(t, addr)

	// The four profiles that answer the question this server exists for. cmdline
	// and symbol come along with Index; heap is the one that attributes a live
	// heap, goroutine catches a leaked-goroutine heap.
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
		"/debug/pprof/cmdline",
	} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

// Importing net/http/pprof registers its handlers on http.DefaultServeMux as an
// unconditional import side effect. Serving them from a private mux does not
// undo that -- nothing can, short of not importing the package. So the property
// worth defending is not "the default mux is clean" but "no server in this
// binary serves the default mux", and the way that gets broken is a nil handler.
//
// This scans the source for the two shapes that pick up DefaultServeMux by
// omission. It is a cheap guard against a real future mistake: today it costs a
// contributor nothing, and the day someone writes ListenAndServe(addr, nil) it
// turns a silent heap-dump endpoint into a failed test.
func TestNoServerUsesDefaultServeMux(t *testing.T) {
	root := repoRoot(t)
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "proto", "temp":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.Contains(trimmed, "http.ListenAndServe(") && strings.HasSuffix(trimmed, ", nil)") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, trimmed))
			}
			if strings.HasPrefix(trimmed, "Handler:") && strings.Contains(trimmed, "nil") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, trimmed))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(offenders) > 0 {
		t.Fatalf("server(s) fall back to http.DefaultServeMux, which net/http/pprof "+
			"has populated with heap-dump handlers:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never listened on %s", addr)
}
