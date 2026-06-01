package stream

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

const testBufSize = 1 << 20

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Agent.TenantToken = "tenant-secret"
	cfg.Agent.AgentID = "agent-1"
	cfg.Agent.ClusterID = "cluster-1"
	cfg.Gateway.Address = "passthrough:///bufnet"
	cfg.Gateway.TLS = false
	cfg.Gateway.ReconnectInitialDelay = 10 * time.Millisecond
	cfg.Gateway.ReconnectMaxDelay = 100 * time.Millisecond
	cfg.Gateway.HandshakeTimeout = 2 * time.Second
	return cfg
}

func newTestQueue(t *testing.T) queue.Queue {
	t.Helper()
	reg := prometheus.NewRegistry()
	q, err := queue.New(&config.BufferConfig{
		MaxMemoryBytes: 1 << 20,
		BatchSize:      10,
	}, logger.New("queue-test"), reg)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func startBufGRPCServer(t *testing.T, srv agentv1.AgentServiceServer) (*grpc.Server, *bufconn.Listener) {
	t.Helper()
	lis := bufconn.Listen(testBufSize)
	s := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(s, srv)
	go func() {
		_ = s.Serve(lis)
	}()
	t.Cleanup(func() {
		s.Stop()
		_ = lis.Close()
	})
	return s, lis
}

func dialBufnet(ctx context.Context, lis *bufconn.Listener, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	base := []grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	return grpc.NewClient("passthrough:///bufnet", append(base, opts...)...)
}

func newTestManager(t *testing.T, cfg *config.Config, q queue.Queue, lis *bufconn.Listener) (*streamManager, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	mgr, err := New(cfg, q, logger.New("stream-test"), reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sm := mgr.(*streamManager)
	sm.dial = func(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
		ic := interceptorDeps{cfg: func() *config.Config { return cfg }, log: sm.log, metrics: sm.metrics}
		conn, err := dialBufnet(ctx, lis,
			grpc.WithChainUnaryInterceptor(ic.chainUnary()...),
			grpc.WithChainStreamInterceptor(ic.chainStream()...),
		)
		if err != nil {
			return nil, nil, err
		}
		return conn, agentv1.NewAgentServiceClient(conn), nil
	}
	return sm, reg
}

func gaugeState(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	g, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range g {
		if mf.GetName() != "kubexa_grpc_connection_state" {
			continue
		}
		metrics := mf.GetMetric()
		if len(metrics) == 0 {
			t.Fatal("connection_state has no samples")
		}
		return metrics[0].GetGauge().GetValue()
	}
	t.Fatal("connection_state metric not found")
	return 0
}

func TestStateMachineTransitions(t *testing.T) {
	t.Parallel()

	var connectCount atomic.Int32
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			connectCount.Add(1)
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-1"},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	q := newTestQueue(t)
	sm, reg := newTestManager(t, cfg, q, lis)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.Connected() })
	if got := sm.SessionID(); got != "sess-1" {
		t.Fatalf("SessionID() = %q, want sess-1", got)
	}
	if v := gaugeState(t, reg); v != stateGaugeValue(StateReady) {
		t.Fatalf("gauge state = %v, want ready(%v)", v, stateGaugeValue(StateReady))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit")
	}
	if sm.currentState() != StateShutdown {
		t.Fatalf("final state = %s, want shutdown", sm.currentState())
	}
}

type mockGateway struct {
	agentv1.UnimplementedAgentServiceServer
	onConnect func(grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error
}

func (m *mockGateway) Connect(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
	if m.onConnect != nil {
		return m.onConnect(stream)
	}
	return status.Error(codes.Unimplemented, "not configured")
}

func TestHandshakeSuccess(t *testing.T) {
	t.Parallel()

	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			hs := msg.GetHandshake()
			if hs == nil {
				return status.Error(codes.InvalidArgument, "expected handshake")
			}
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{
						Accepted:      true,
						SessionId:     "session-abc",
						ServerVersion: "gw-1",
						Config:        &agentv1.ConfigSnapshot{BatchSize: 50},
					},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.SessionID() == "session-abc" })
	snap := sm.ConfigSnapshot()
	if snap == nil || snap.GetBatchSize() != 50 {
		t.Fatalf("ConfigSnapshot = %+v, want batch_size 50", snap)
	}
}

func TestHandshakeRejection(t *testing.T) {
	t.Parallel()

	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{
						Accepted:         false,
						RejectionReason:  "invalid tenant",
					},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := sm.Run(ctx)
	if err == nil {
		t.Fatal("expected permanent handshake error")
	}
	var perm *permanentGatewayError
	if !errors.As(err, &perm) {
		t.Fatalf("error type = %T, want *permanentGatewayError", err)
	}
	if sm.currentState() != StateShutdown {
		t.Fatalf("state = %s, want shutdown", sm.currentState())
	}
}

func TestCircuitBreakerPermanentGRPCError(t *testing.T) {
	t.Parallel()

	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			return status.Error(codes.Unauthenticated, "bad token")
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sm.Run(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if sm.currentState() != StateShutdown {
		t.Fatalf("state = %s, want shutdown", sm.currentState())
	}
}

func TestBackpressureThrottle(t *testing.T) {
	t.Parallel()

	backpressureSent := make(chan struct{})
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "s1"},
				},
			}); err != nil {
				return err
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Backpressure{
					Backpressure: &agentv1.BackpressureSignal{Throttle: true, DelayMs: 200},
				},
			}); err != nil {
				return err
			}
			close(backpressureSent)
			<-stream.Context().Done()
			return stream.Context().Err()
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)
	now := time.Now()
	sm.throttle.clock = func() time.Time { return now }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.Connected() })
	<-backpressureSent
	sm.throttle.pause(300 * time.Millisecond)
	if !sm.IsThrottled() {
		t.Fatal("expected throttled")
	}
	now = now.Add(400 * time.Millisecond)
	if sm.IsThrottled() {
		t.Fatal("expected throttle lifted after delay")
	}
	cancel()
}

func TestReconnectBackoffJitter(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			n := attempts.Add(1)
			if n < 3 {
				return status.Error(codes.Unavailable, "try again")
			}
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "ok"},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)
	sm.rng = rand.New(rand.NewSource(42)) //nolint:gosec

	var slept []time.Duration
	sm.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return sm.Connected() })
	if len(slept) < 2 {
		t.Fatalf("expected at least 2 backoff sleeps, got %d", len(slept))
	}
	min := time.Duration(float64(cfg.Gateway.ReconnectInitialDelay) * (1 - backoffJitterFraction))
	max := time.Duration(float64(cfg.Gateway.ReconnectInitialDelay) * (1 + backoffJitterFraction))
	if slept[0] < min || slept[0] > max {
		t.Fatalf("first sleep = %v, want between %v and %v", slept[0], min, max)
	}
	cancel()
}

func TestBackoffJitterDeterministic(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(42)) //nolint:gosec
	initial := 10 * time.Millisecond
	max := 100 * time.Millisecond
	d0 := backoff(0, initial, max, rng)
	d1 := backoff(1, initial, max, rng)
	if d0 < 8*time.Millisecond || d0 > 12*time.Millisecond {
		t.Fatalf("attempt 0 delay %v out of jitter range", d0)
	}
	if d1 <= d0 {
		t.Fatalf("expected attempt 1 delay (%v) > attempt 0 (%v) before cap", d1, d0)
	}
}

func TestAuthInterceptorMetadata(t *testing.T) {
	t.Parallel()

	var gotMD metadata.MD
	ic := interceptorDeps{
		cfg: func() *config.Config {
			cfg := testConfig()
			cfg.Agent.TenantToken = "rotating-token"
			return cfg
		},
		log: logger.New("auth-test"),
	}

	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		gotMD, _ = metadata.FromOutgoingContext(ctx)
		return nil
	}

	err := ic.authUnary()(context.Background(), "/test.Service/Method", nil, nil, nil, invoker)
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	assertMD(t, gotMD, "x-tenant-token", "rotating-token")
	assertMD(t, gotMD, "x-agent-version", AgentVersion)
	assertMD(t, gotMD, "x-proto-version", protoVersion)
	assertMD(t, gotMD, "x-cluster-id", "cluster-1")
}

func TestRecoveryInterceptorCatchesPanic(t *testing.T) {
	t.Parallel()

	ic := interceptorDeps{log: logger.New("recovery-test")}
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		panic("boom")
	}
	err := ic.recoveryUnary()(context.Background(), "/test.Service/Panic", nil, nil, nil, invoker)
	if err == nil {
		t.Fatal("expected error from panic")
	}
}

func TestSendBuffersWhenNotConnected(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	q := newTestQueue(t)
	sm, _ := newTestManager(t, cfg, q, bufconn.Listen(testBufSize))

	msg := &agentv1.AgentMessage{MessageId: "m1", Payload: &agentv1.AgentMessage_Heartbeat{Heartbeat: &agentv1.Heartbeat{}}}
	if err := sm.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if q.Depth() != 1 {
		t.Fatalf("queue depth = %d, want 1", q.Depth())
	}
}

func TestRedactMetadata(t *testing.T) {
	t.Parallel()
	md := metadata.Pairs("x-tenant-token", "secret", "x-cluster-id", "c1")
	red := redactMetadata(md)
	assertMD(t, red, "x-tenant-token", "***")
	assertMD(t, red, "x-cluster-id", "c1")
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func assertMD(t *testing.T, md metadata.MD, key, want string) {
	t.Helper()
	vals := md.Get(key)
	if len(vals) != 1 || vals[0] != want {
		t.Fatalf("metadata %q = %v, want %q", key, vals, want)
	}
}

func TestClassifyGRPCError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code      codes.Code
		permanent bool
		transient bool
	}{
		{codes.Unauthenticated, true, false},
		{codes.PermissionDenied, true, false},
		{codes.Unimplemented, true, false},
		{codes.Unavailable, false, true},
		{codes.DeadlineExceeded, false, true},
		{codes.ResourceExhausted, false, true},
	}
	for _, tc := range cases {
		err := status.Error(tc.code, "test")
		p, tr := classifyGRPCError(err)
		if p != tc.permanent || tr != tc.transient {
			t.Fatalf("%s: permanent=%v transient=%v", tc.code, p, tr)
		}
	}
}

func TestConsecutiveTransientCriticalLog(t *testing.T) {
	t.Parallel()
	cb := &circuitBreaker{}
	for i := 0; i < maxConsecutiveTransientFailures; i++ {
		if n := cb.recordTransient(); n != i+1 {
			t.Fatalf("record %d: got %d", i, n)
		}
	}
	cb.recordSuccess()
	if cb.consecutiveTransient != 0 {
		t.Fatal("expected reset after success")
	}
}
