package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The agent builds its own registry rather than using the default one, and a
// fresh registry carries no Go or process collectors. That is the whole reason
// a v0.6.0 agent could be OOMKilled with no go_memstats_* to look at.
func TestRegisterRuntimeCollectorsExposesHeapAndRSS(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterRuntimeCollectors(reg); err != nil {
		t.Fatalf("RegisterRuntimeCollectors: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := make(map[string]struct{}, len(families))
	for _, f := range families {
		seen[f.GetName()] = struct{}{}
	}

	// heap_alloc is the live heap and next_gc is the goal the runtime is
	// steering toward -- the pair that separates "the heap grew" from "the GC
	// was aiming high". resident_memory is what the kernel counts against the
	// container limit; without it there is no way to tell heap growth from
	// anything else in the process.
	for _, want := range []string{
		"go_memstats_heap_alloc_bytes",
		"go_memstats_next_gc_bytes",
		"go_goroutines",
		"process_resident_memory_bytes",
		"process_cpu_seconds_total",
	} {
		if _, ok := seen[want]; !ok {
			t.Errorf("missing %q; gathered: %s", want, strings.Join(keys(seen), ", "))
		}
	}
}

func TestRegisterRuntimeCollectorsIsNotIdempotent(t *testing.T) {
	// Registering twice on one registry is a duplicate-collector error, not a
	// silent no-op. A caller that wires this up in two places must find out.
	reg := prometheus.NewRegistry()
	if err := RegisterRuntimeCollectors(reg); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := RegisterRuntimeCollectors(reg); err == nil {
		t.Fatal("second register returned nil error, want a duplicate registration error")
	}
}

func TestRegisterRuntimeCollectorsRejectsNilRegistry(t *testing.T) {
	if err := RegisterRuntimeCollectors(nil); err == nil {
		t.Fatal("nil registry accepted, want an error")
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The metrics listener is the one this agent actually publishes on the Service,
// so it is the one that must never answer a profile request. net/http/pprof is
// linked into this binary and has populated http.DefaultServeMux with heap-dump
// handlers; this asserts the metrics server's own mux is not that one.
func TestMetricsServerDoesNotServeProfiles(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterRuntimeCollectors(reg); err != nil {
		t.Fatalf("RegisterRuntimeCollectors: %v", err)
	}
	srv := NewServer("127.0.0.1:0", reg, nil)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap"} {
		rec := httptest.NewRecorder()
		srv.httpSrv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("metrics server answered %s with 200; profiles must not be reachable "+
				"on the published metrics port", path)
		}
	}

	// The same handler must still serve what it is for.
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "go_memstats_heap_alloc_bytes") {
		t.Error("/metrics does not expose go_memstats_heap_alloc_bytes")
	}
}
