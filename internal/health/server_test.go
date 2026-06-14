package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/metrics"
)

type staticChecker struct {
	name string
	err  error
	wait time.Duration
}

func (c *staticChecker) Name() string { return c.name }

func (c *staticChecker) Check(ctx context.Context) error {
	if c.wait > 0 {
		timer := time.NewTimer(c.wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return c.err
}

func startTestServer(t *testing.T, srv *Server) (addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	srv.cfg.Addr = lis.Addr().String()
	srv.ln = lis
	srv.httpSrv.Addr = lis.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.httpSrv.Serve(lis)
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()
		_ = srv.httpSrv.Shutdown(shutdownCtx)
	}()

	waitForHealth(t, lis.Addr().String(), srv.cfg.LivenessPath)
	return lis.Addr().String(), cancel, errCh
}

func waitForHealth(t *testing.T, addr, path string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	url := "http://" + addr + path
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("health server at %s did not become ready", addr)
}

func TestLivenessAlwaysOK(t *testing.T) {
	t.Parallel()

	srv := New(DefaultConfig(), logger.New("health-test"), nil)
	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	resp, err := http.Get("http://" + addr + srv.cfg.LivenessPath)
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID header")
	}

	var body livenessResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
	if body.Timestamp.IsZero() {
		t.Fatal("timestamp is zero")
	}

	cancel()
	waitServerDone(t, done)
}

func TestReadinessAllHealthy(t *testing.T) {
	t.Parallel()

	srv := New(DefaultConfig(), logger.New("health-test"), nil)
	srv.Register(&staticChecker{name: "k8s"})
	srv.Register(&staticChecker{name: "gateway"})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	resp, err := http.Get("http://" + addr + srv.cfg.ReadinessPath)
	if err != nil {
		t.Fatalf("GET /readyz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body readinessResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("overall status = %q, want ok", body.Status)
	}
	if body.Components != nil {
		t.Fatalf("components = %#v, want omitted on success", body.Components)
	}

	cancel()
	waitServerDone(t, done)
}

func TestReadinessAnyUnhealthy(t *testing.T) {
	t.Parallel()

	srv := New(DefaultConfig(), logger.New("health-test"), nil)
	srv.Register(&staticChecker{name: "k8s"})
	srv.Register(&staticChecker{name: "gateway", err: Degraded(errors.New("connection refused"))})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	resp, err := http.Get("http://" + addr + srv.cfg.ReadinessPath)
	if err != nil {
		t.Fatalf("GET /readyz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	var body readinessResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "degraded" {
		t.Fatalf("overall status = %q, want degraded", body.Status)
	}
	if body.Components["gateway"].Status != "degraded" {
		t.Fatalf("gateway status = %q, want degraded", body.Components["gateway"].Status)
	}
	if body.Components["gateway"].Error == nil || *body.Components["gateway"].Error != "connection refused" {
		t.Fatalf("gateway error = %v, want connection refused", body.Components["gateway"].Error)
	}

	cancel()
	waitServerDone(t, done)
}

func TestReadinessConcurrentChecks(t *testing.T) {
	t.Parallel()

	const checkDelay = 200 * time.Millisecond
	srv := New(HealthConfig{Timeout: 2 * time.Second}, logger.New("health-test"), nil)
	for i := 0; i < 3; i++ {
		srv.Register(&staticChecker{
			name: fmt.Sprintf("c%d", i),
			wait: checkDelay,
		})
	}

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	start := time.Now()
	resp, err := http.Get("http://" + addr + srv.cfg.ReadinessPath)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET /readyz error = %v", err)
	}
	_ = resp.Body.Close()

	if elapsed >= 3*checkDelay {
		t.Fatalf("checks appear sequential: elapsed %s >= %s", elapsed, 3*checkDelay)
	}

	cancel()
	waitServerDone(t, done)
}

func TestReadinessCheckerTimeout(t *testing.T) {
	t.Parallel()

	srv := New(HealthConfig{Timeout: 50 * time.Millisecond}, logger.New("health-test"), nil)
	srv.Register(&staticChecker{name: "slow", wait: 500 * time.Millisecond})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	resp, err := http.Get("http://" + addr + srv.cfg.ReadinessPath)
	if err != nil {
		t.Fatalf("GET /readyz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	var body readinessResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Components["slow"].Status != "unhealthy" {
		t.Fatalf("slow status = %q, want unhealthy", body.Components["slow"].Status)
	}
	if body.Components["slow"].Error == nil || !strings.Contains(*body.Components["slow"].Error, "context deadline exceeded") {
		t.Fatalf("slow error = %v, want deadline exceeded", body.Components["slow"].Error)
	}

	cancel()
	waitServerDone(t, done)
}

func TestComponentStateTransitionLogging(t *testing.T) {
	var logBuf bytes.Buffer
	log := logger.New("health-test", logger.WithWriter(&logBuf), logger.WithLevel(logger.LevelInfo))

	srv := New(DefaultConfig(), log, nil)
	srv.Register(&staticChecker{name: "k8s", err: Unhealthy(errors.New("down"))})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	if _, err := http.Get("http://" + addr + srv.cfg.ReadinessPath); err != nil {
		t.Fatalf("first GET: %v", err)
	}
	first := logBuf.String()
	if !strings.Contains(first, "component health degraded") {
		t.Fatalf("first check logs = %q, want degraded transition", first)
	}

	logBuf.Reset()
	if _, err := http.Get("http://" + addr + srv.cfg.ReadinessPath); err != nil {
		t.Fatalf("second GET: %v", err)
	}
	if strings.Contains(logBuf.String(), "component health") {
		t.Fatalf("second check should not log transition, got %q", logBuf.String())
	}

	srv.mu.Lock()
	srv.checkers["k8s"] = &staticChecker{name: "k8s"}
	srv.mu.Unlock()

	logBuf.Reset()
	if _, err := http.Get("http://" + addr + srv.cfg.ReadinessPath); err != nil {
		t.Fatalf("third GET: %v", err)
	}
	if !strings.Contains(logBuf.String(), "component health recovered") {
		t.Fatalf("recovery logs = %q, want recovered transition", logBuf.String())
	}

	cancel()
	waitServerDone(t, done)
}

func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	t.Parallel()

	var active int32
	block := make(chan struct{})

	srv := New(DefaultConfig(), logger.New("health-test"), nil)
	srv.Register(&blockingChecker{
		name:   "block",
		block:  block,
		active: &active,
	})

	addr, cancel, done := startTestServer(t, srv)

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + addr + srv.cfg.ReadinessPath)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&active) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&active) == 0 {
		t.Fatal("in-flight readiness request did not start")
	}

	cancel()
	close(block)

	select {
	case <-reqDone:
	case <-time.After(6 * time.Second):
		t.Fatal("in-flight request was not drained before shutdown timeout")
	}

	waitServerDone(t, done)
}

type blockingChecker struct {
	name   string
	block  <-chan struct{}
	active *int32
}

func (c *blockingChecker) Name() string { return c.name }

func (c *blockingChecker) Check(ctx context.Context) error {
	atomic.StoreInt32(c.active, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.block:
		return nil
	}
}

func TestVerboseQueryParameter(t *testing.T) {
	t.Parallel()

	srv := New(DefaultConfig(), logger.New("health-test"), nil)
	srv.Register(&staticChecker{name: "k8s"})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	resp, err := http.Get("http://" + addr + srv.cfg.ReadinessPath + "?verbose=true")
	if err != nil {
		t.Fatalf("GET verbose readyz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body readinessResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Components == nil || body.Components["k8s"].Status != "ok" {
		t.Fatalf("components = %#v, want k8s ok with verbose=true", body.Components)
	}

	cancel()
	waitServerDone(t, done)
}

func TestReadinessUpdatesHealthMetrics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg, "test", "cluster", "agent")
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	healthMetrics := m.Health()

	srv := New(DefaultConfig(), logger.New("health-test"), healthMetrics)
	srv.Register(&staticChecker{name: "k8s"})
	srv.Register(&staticChecker{name: "gateway", err: Unhealthy(errors.New("down"))})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	if _, err := http.Get("http://" + addr + srv.cfg.ReadinessPath); err != nil {
		t.Fatalf("GET /readyz error = %v", err)
	}

	if v := gaugeValue(t, reg, "kubexa_health_status", map[string]string{"component": "k8s"}); v != 1 {
		t.Fatalf("k8s health metric = %v, want 1", v)
	}
	if v := gaugeValue(t, reg, "kubexa_health_status", map[string]string{"component": "gateway"}); v != 0 {
		t.Fatalf("gateway health metric = %v, want 0", v)
	}

	cancel()
	waitServerDone(t, done)
}

func TestRegisterOverwriteDuplicate(t *testing.T) {
	t.Parallel()

	srv := New(DefaultConfig(), logger.New("health-test"), nil)
	srv.Register(&staticChecker{name: "k8s", err: Unhealthy(errors.New("old"))})
	srv.Register(&staticChecker{name: "k8s"})

	addr, cancel, done := startTestServer(t, srv)
	defer cancel()

	resp, err := http.Get("http://" + addr + srv.cfg.ReadinessPath)
	if err != nil {
		t.Fatalf("GET /readyz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after overwrite", resp.StatusCode)
	}

	cancel()
	waitServerDone(t, done)
}

func waitServerDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop within timeout")
	}
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()

	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, mf := range gathered {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if !labelSetMatch(metric.GetLabel(), labels) {
				continue
			}
			return metric.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

func labelSetMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, lp := range pairs {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func TestStartReturnsListenErrorWhenPortInUse(t *testing.T) {
	t.Parallel()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = occupied.Close() }()

	srv := New(HealthConfig{Addr: occupied.Addr().String()}, logger.New("health-test"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	startErr := srv.Start(ctx)
	if startErr == nil {
		t.Fatal("Start() error = nil, want listen failure")
	}
	if !strings.Contains(startErr.Error(), "listen") {
		t.Fatalf("Start() error = %v, want listen failure", startErr)
	}
}

func TestStartGracefulShutdown(t *testing.T) {
	t.Parallel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	srv := New(HealthConfig{Addr: addr}, logger.New("health-test"), nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx)
	}()

	waitForHealth(t, addr, srv.cfg.LivenessPath)
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() error = %v, want context.Canceled", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Start() did not return after cancel")
	}
}
