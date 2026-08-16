package stream

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/pkg/buildinfo"
	"github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/protoversion"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
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

func newTestAgentMetrics(t *testing.T, reg *prometheus.Registry) (*agentmetrics.Metrics, *agentmetrics.StreamMetrics, *agentmetrics.ConnectionMetrics) {
	t.Helper()
	m, err := agentmetrics.New(reg, "test", "cluster-1", "agent-1")
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return m, m.Stream(), m.Connection()
}

func newTestQueue(t *testing.T) queue.Queue {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := agentmetrics.New(reg, "test", "cluster-1", "agent-1")
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	q, err := queue.New(&config.BufferConfig{
		MaxMemoryBytes: 1 << 20,
		BatchSize:      10,
	}, logger.New("queue-test"), m.Queue())
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
	_, streamMetrics, connMetrics := newTestAgentMetrics(t, reg)
	mgr, err := New(cfg, q, logger.New("stream-test"), streamMetrics, connMetrics, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sm := mgr.(*streamManager)
	sm.dial = func(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
		ic := interceptorDeps{cfg: func() *config.Config { return cfg }, log: sm.log, streamMetrics: sm.streamMetrics}
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

func gaugeState(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	g, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range g {
		if mf.GetName() != "kubexa_connection_state" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if metric.GetGauge().GetValue() != 1 {
				continue
			}
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "state" {
					return lp.GetValue()
				}
			}
		}
		t.Fatal("connection_state has no active state sample")
	}
	t.Fatal("connection_state metric not found")
	return ""
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
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-1"},
				},
			}); err != nil {
				return err
			}
			return holdGatewayStream(stream)
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
	if state := gaugeState(t, reg); state != StateReady.String() {
		t.Fatalf("connection state = %q, want %q", state, StateReady.String())
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

// TestReconnectAfterGatewayStreamDrop verifies the agent reconnects when the
// gateway closes the stream (e.g. process restart) while sendLoop is idle.
func TestReconnectAfterGatewayStreamDrop(t *testing.T) {
	t.Parallel()

	var connects atomic.Int32
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			n := connects.Add(1)
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{
						Accepted:  true,
						SessionId: fmt.Sprintf("sess-%d", n),
					},
				},
			}); err != nil {
				return err
			}
			if n == 1 {
				return nil
			}
			<-stream.Context().Done()
			return stream.Context().Err()
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool {
		return connects.Load() >= 2 && sm.Connected()
	})
	if got := connects.Load(); got < 2 {
		t.Fatalf("gateway connect attempts = %d, want >= 2", got)
	}
	if got := sm.SessionID(); got != "sess-2" {
		t.Fatalf("SessionID() after reconnect = %q, want sess-2", got)
	}

	cancel()
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

// holdGatewayStream keeps a mock gateway session open until the client disconnects.
func holdGatewayStream(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
	<-stream.Context().Done()
	return stream.Context().Err()
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
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{
						Accepted:      true,
						SessionId:     "session-abc",
						ServerVersion: "gw-1",
						Config:        &agentv1.ConfigSnapshot{BatchSize: 50},
					},
				},
			}); err != nil {
				return err
			}
			return holdGatewayStream(stream)
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
						Accepted:        false,
						RejectionReason: "cluster not registered",
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

func TestHandshakeInvalidTenantTokenRetriesUntilAccepted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			n := attempts.Add(1)
			if n < 3 {
				return stream.Send(&agentv1.GatewayMessage{
					Payload: &agentv1.GatewayMessage_Handshake{
						Handshake: &agentv1.HandshakeResponse{
							Accepted:        false,
							RejectionReason: recoverableRejectionInvalidTenantToken,
						},
					},
				})
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{
						Accepted:  true,
						SessionId: "sess-token-retry",
					},
				},
			}); err != nil {
				return err
			}
			return holdGatewayStream(stream)
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool {
		return sm.SessionID() == "sess-token-retry"
	})

	if got := attempts.Load(); got < 3 {
		t.Fatalf("handshake attempts = %d, want >= 3", got)
	}

	cancel()
	<-done
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
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "ok"},
				},
			}); err != nil {
				return err
			}
			return holdGatewayStream(stream)
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
	assertMD(t, gotMD, "x-agent-version", buildinfo.Version)
	assertMD(t, gotMD, "x-proto-version", protoversion.Current)
	assertMD(t, gotMD, "x-cluster-id", "cluster-1")
}

// Deliberately NOT t.Parallel(): this test mutates the package global
// buildinfo.Version, which TestAuthInterceptorMetadata asserts against. With
// both marked parallel they ran concurrently and the assertion read
// "1.2.3-test" roughly once in 300 runs. Staying sequential is what fixes it:
// a parallel sibling is paused for the whole sequential phase, so it cannot
// observe the mutation window.
func TestAuthInterceptorMetadataUsesBuildinfoVersion(t *testing.T) {
	orig := buildinfo.Version
	buildinfo.Version = "1.2.3-test"
	t.Cleanup(func() { buildinfo.Version = orig })

	var gotMD metadata.MD
	ic := interceptorDeps{
		cfg: func() *config.Config { return testConfig() },
		log: logger.New("auth-test"),
	}
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		gotMD, _ = metadata.FromOutgoingContext(ctx)
		return nil
	}
	if err := ic.authUnary()(context.Background(), "/test.Service/Method", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	assertMD(t, gotMD, "x-agent-version", "1.2.3-test")
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

func TestDrainBufferedQueueAfterConnect(t *testing.T) {
	t.Parallel()

	var received sync.Map
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			if msg.GetHandshake() == nil {
				return status.Error(codes.InvalidArgument, "expected handshake")
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-drain"},
				},
			}); err != nil {
				return err
			}

			for {
				msg, err := stream.Recv()
				if err != nil {
					return err
				}
				received.Store(msg.GetMessageId(), msg)
			}
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	q := newTestQueue(t)
	sm, _ := newTestManager(t, cfg, q, lis)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.Connected() })

	logMsg := &agentv1.AgentMessage{
		MessageId: "queued-log-1",
		Payload: &agentv1.AgentMessage_Logs{
			Logs: &agentv1.LogBatch{
				Entries: []*agentv1.LogEntry{
					{Namespace: "stage", PodName: "be-1", Message: "hello from queue"},
				},
			},
		},
	}
	payload, err := proto.Marshal(logMsg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := q.Enqueue(context.Background(), queue.Item{ID: logMsg.MessageId, Payload: payload}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		_, ok := received.Load("queued-log-1")
		return ok
	})
	if q.Depth() != 0 {
		t.Fatalf("queue depth = %d, want 0 after drain", q.Depth())
	}

	cancel()
}

func TestDrainBufferedQueueSkipsLogsWhenDisabled(t *testing.T) {
	t.Parallel()

	var received sync.Map
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			if msg.GetHandshake() == nil {
				return status.Error(codes.InvalidArgument, "expected handshake")
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-skip-logs"},
				},
			}); err != nil {
				return err
			}

			for {
				msg, err := stream.Recv()
				if err != nil {
					return err
				}
				received.Store(msg.GetMessageId(), msg)
			}
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	cfg.Collect.Logs.Enabled = false
	q := newTestQueue(t)
	sm, _ := newTestManager(t, cfg, q, lis)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.Connected() })

	logMsg := &agentv1.AgentMessage{
		MessageId: "queued-log-disabled",
		Payload: &agentv1.AgentMessage_Logs{
			Logs: &agentv1.LogBatch{
				Entries: []*agentv1.LogEntry{
					{Namespace: "stage", PodName: "be-1", Message: "should not be delivered"},
				},
			},
		},
	}
	payload, err := proto.Marshal(logMsg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := q.Enqueue(context.Background(), queue.Item{ID: logMsg.MessageId, Payload: payload}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool { return q.Depth() == 0 })

	if _, ok := received.Load("queued-log-disabled"); ok {
		t.Fatal("log message was delivered while collect.logs.enabled is false")
	}

	cancel()
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

// fakeConnectClient stands in for the generated stream client. Only Send is
// reached by drainBufferedQueue, so the embedded interface is left nil: any
// other call would panic loudly rather than silently pass.
type fakeConnectClient struct {
	agentv1.AgentService_ConnectClient
	mu        sync.Mutex
	sent      int
	failAfter int
	// stopAfter, when positive, calls stop once that many sends have been
	// attempted. It ends the drain loop from the outside so a test that is
	// asserting on what the loop settled cannot hang on a loop that keeps
	// re-dequeuing what it just nacked.
	stopAfter int
	stop      func()
}

func (f *fakeConnectClient) Send(*agentv1.AgentMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	if f.stopAfter > 0 && f.sent == f.stopAfter && f.stop != nil {
		f.stop()
	}
	if f.sent > f.failAfter {
		return errors.New("stream broken")
	}
	return nil
}

func (f *fakeConnectClient) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}

// inflightObserver is how a test reads the queue's inflight map: InflightLen
// is deliberately absent from the Queue interface.
type inflightObserver interface{ InflightLen() int }

func inflightLen(t *testing.T, q queue.Queue) int {
	t.Helper()
	observer, ok := q.(inflightObserver)
	if !ok {
		t.Fatal("queue does not expose InflightLen")
	}
	return observer.InflightLen()
}

// enqueueTestLogs puts n marshalled log messages on q, id-0..id-(n-1).
func enqueueTestLogs(t *testing.T, ctx context.Context, q queue.Queue, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		msg := &agentv1.AgentMessage{
			MessageId: fmt.Sprintf("id-%d", i),
			Payload: &agentv1.AgentMessage_Logs{
				Logs: &agentv1.LogBatch{
					Entries: []*agentv1.LogEntry{
						{Namespace: "stage", PodName: "be-1", Message: "queued"},
					},
				},
			},
		}
		payload, err := proto.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal(%d): %v", i, err)
		}
		if err := q.Enqueue(ctx, queue.Item{ID: msg.MessageId, Payload: payload}); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
	}
}

func newDrainTestManager(t *testing.T, cfg *config.Config, q queue.Queue) *streamManager {
	t.Helper()
	reg := prometheus.NewRegistry()
	_, streamMetrics, connMetrics := newTestAgentMetrics(t, reg)
	mgr, err := New(cfg, q, logger.New("stream-test"), streamMetrics, connMetrics, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sm := mgr.(*streamManager)
	sm.ready.Store(true)
	return sm
}

// A send that fails partway through a batch must leave nothing in flight:
// items already on the wire get acked, the failed item and every item after
// it get nacked. Returning early skipped both, stranding them in the queue's
// inflight map until the process restarted -- and with WAL compaction a single
// stranded item pins its segment forever.
func TestDrainNacksTheRemainderOfAFailedBatch(t *testing.T) {
	t.Parallel()

	const batchSize = 6
	const failAfter = 3

	cfg := testConfig()
	cfg.Buffer.BatchSize = batchSize
	q := newTestQueue(t)
	sm := newDrainTestManager(t, cfg, q)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	enqueueTestLogs(t, ctx, q, batchSize)

	sm.drainBufferedQueue(ctx, &fakeConnectClient{failAfter: failAfter})

	if got := inflightLen(t, q); got != 0 {
		t.Errorf("%d items left in flight after a failed send, want 0", got)
	}
	if got := q.Depth(); got != batchSize-failAfter {
		t.Errorf("Depth() = %d, want %d (the failed item and everything after it)",
			got, batchSize-failAfter)
	}
}

// ackFailingQueue is a real queue whose Ack always fails, standing in for the
// one failure that matters here: appendAck hitting a full disk. Ack keeps the
// inflight entry when it cannot persist -- correct at the queue layer, since
// the entry is what justifies holding the segment claim -- so it is the
// caller's job to make sure something still settles the batch.
type ackFailingQueue struct {
	queue.Queue
	ackCalls  atomic.Int32
	nackCalls atomic.Int32
	nackIDs   atomic.Int32
}

func (a *ackFailingQueue) Ack(ids []string) error {
	a.ackCalls.Add(1)
	return errors.New("persist ack: no space left on device")
}

func (a *ackFailingQueue) Nack(ids []string) error {
	a.nackCalls.Add(1)
	a.nackIDs.Add(int32(len(ids)))
	return a.Queue.Nack(ids)
}

// Every send succeeds, so the whole batch is acked -- and every ack fails.
// The queue keeps those entries inflight on purpose; if the caller then walks
// away, they are stranded for the process lifetime, holding their payloads and
// pinning their WAL segments. The drain must settle them anyway, and must not
// retry a permanently failing ack forever.
func TestDrainSettlesABatchWhoseAckCannotBePersisted(t *testing.T) {
	t.Parallel()

	const batchSize = 4

	cfg := testConfig()
	cfg.Buffer.BatchSize = batchSize
	inner := newTestQueue(t)
	q := &ackFailingQueue{Queue: inner}
	sm := newDrainTestManager(t, cfg, q)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	enqueueTestLogs(t, ctx, q, batchSize)

	// Stop the loop from the send side once the batch is on the wire: the
	// batch is nacked back into the queue, so a loop left running would
	// re-dequeue it forever.
	stream := &fakeConnectClient{failAfter: batchSize, stopAfter: batchSize, stop: cancel}
	sm.drainBufferedQueue(ctx, stream)

	if got := inflightLen(t, inner); got != 0 {
		t.Errorf("%d items left in flight after a failed ack, want 0", got)
	}
	if got := q.Depth(); got != batchSize {
		t.Errorf("Depth() = %d, want %d (an unpersisted ack must not lose the items)", got, batchSize)
	}
	if got := q.nackCalls.Load(); got != 1 {
		t.Errorf("Nack called %d times, want 1", got)
	}
	if got := q.nackIDs.Load(); got != batchSize {
		t.Errorf("Nack covered %d ids, want %d", got, batchSize)
	}
	// The bound: a full disk fails every ack, so the retry must give up.
	if got := q.ackCalls.Load(); got < 1 || got > 3 {
		t.Errorf("Ack called %d times, want a bounded 1..3", got)
	}
	if got := stream.sendCount(); got != batchSize {
		t.Errorf("sent %d messages, want %d", got, batchSize)
	}
}
