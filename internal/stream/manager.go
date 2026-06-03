// Package stream manages the outbound gRPC connection from kubexa-agent to the Kubexa Gateway.
package stream

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

const (
	defaultSendChannelSize = 256
	handshakeMsgTimeout    = 10 * time.Second
)

// ErrSendQueueFull is returned when the internal send buffer is saturated.
var ErrSendQueueFull = errors.New("stream send queue full")

// Manager manages the outbound gRPC stream to the Kubexa Gateway.
type Manager interface {
	// Run starts the connection loop. Blocks until ctx is cancelled.
	// Reconnects automatically on failure with exponential backoff.
	Run(ctx context.Context) error

	// Send enqueues an AgentMessage for delivery over the active stream.
	// Returns error if the queue is full or context is done.
	Send(ctx context.Context, msg *agentv1.AgentMessage) error

	// Connected returns true if the bidirectional stream is currently active.
	Connected() bool

	// SessionID returns the current session ID assigned by gateway after handshake.
	// Returns empty string if not connected.
	SessionID() string

	// IsThrottled reports whether Send is paused due to gateway backpressure.
	IsThrottled() bool
}

// streamManager implements Manager.
type streamManager struct {
	cfg    *config.Config
	queue  queue.Queue
	log    *logger.Logger
	metrics *grpcMetrics

	cb      circuitBreaker
	rng     *rand.Rand
	sleep   sleeper
	dial    dialFunc

	mu            sync.Mutex
	state         ConnState
	shutdownErr   error
	sessionID     atomic.Value // string
	ready         atomic.Bool
	configSnap    atomic.Pointer[agentv1.ConfigSnapshot]

	sendCh   chan *agentv1.AgentMessage
	throttle throttleGate

	// active session (set only in StateReady, guarded by sessionMu)
	sessionMu sync.RWMutex
	stream    agentv1.AgentService_ConnectClient
	conn      *grpc.ClientConn

	// signals session goroutines to stop
	sessionCancel context.CancelFunc
	sessionWG     sync.WaitGroup
}

type dialFunc func(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error)

type throttleGate struct {
	mu    sync.RWMutex
	until time.Time
	clock func() time.Time
}

func (g *throttleGate) pause(d time.Duration) {
	if d <= 0 {
		return
	}
	now := time.Now
	if g.clock != nil {
		now = g.clock
	}
	g.mu.Lock()
	g.until = now().Add(d)
	g.mu.Unlock()
}

func (g *throttleGate) throttled() bool {
	now := time.Now
	if g.clock != nil {
		now = g.clock
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return now().Before(g.until)
}

// New constructs a stream Manager wired to cfg, queue, logger, and Prometheus registry.
func New(cfg *config.Config, q queue.Queue, log *logger.Logger, reg prometheus.Registerer) (Manager, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if q == nil {
		return nil, errors.New("queue is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if log == nil {
		log = logger.New("stream")
	}

	metrics, err := newGRPCMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("init stream metrics: %w", err)
	}

	m := &streamManager{
		cfg:     cfg,
		queue:   q,
		log:     log,
		metrics: metrics,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec
		sleep:   defaultSleeper,
		sendCh:  make(chan *agentv1.AgentMessage, defaultSendChannelSize),
		state:   StateIdle,
	}
	m.sessionID.Store("")
	m.dial = m.defaultDial
	metrics.setConnectionState(StateIdle)
	return m, nil
}

// Run implements Manager.
func (m *streamManager) Run(ctx context.Context) error {
	if m == nil {
		return errors.New("stream manager is nil")
	}

	m.log.Info("stream manager starting")
	attempt := 0

	for {
		if err := ctx.Err(); err != nil {
			m.transition(StateShutdown, "context cancelled", nil)
			m.endSession()
			return err
		}

		if m.currentState() == StateShutdown {
			return m.shutdownErr
		}

		m.transition(StateConnecting, "dialing gateway", nil)
		conn, client, err := m.dial(ctx)
		if err != nil {
			permanent, transient := classifyGRPCError(err)
			if permanent {
				m.transition(StateShutdown, "permanent dial failure", err)
				m.shutdownErr = err
				return err
			}
			if transient {
				attempt = m.handleTransientFailure(ctx, attempt, "dial", err)
				continue
			}
			attempt = m.handleTransientFailure(ctx, attempt, "dial", err)
			continue
		}

		sessionCtx, sessionCancel := context.WithCancel(ctx)
		stream, err := client.Connect(sessionCtx)
		if err != nil {
			_ = conn.Close()
			permanent, transient := classifyGRPCError(err)
			sessionCancel()
			if permanent {
				m.transition(StateShutdown, "permanent stream open failure", err)
				m.shutdownErr = err
				return err
			}
			if transient {
				attempt = m.handleTransientFailure(ctx, attempt, "connect", err)
				continue
			}
			attempt = m.handleTransientFailure(ctx, attempt, "connect", err)
			continue
		}

		m.sessionMu.Lock()
		m.conn = conn
		m.stream = stream
		m.sessionCancel = sessionCancel
		m.sessionMu.Unlock()

		m.transition(StateHandshaking, "performing handshake", nil)
		sessionID, err := m.handshake(sessionCtx, stream)
		if err != nil {
			m.endSession()
			permanent, transient := classifyGRPCError(err)
			if permanent {
				m.transition(StateShutdown, "handshake rejected", err)
				m.shutdownErr = err
				return err
			}
			if transient {
				attempt = m.handleTransientFailure(ctx, attempt, "handshake", err)
				continue
			}
			attempt = m.handleTransientFailure(ctx, attempt, "handshake", err)
			continue
		}

		m.sessionID.Store(sessionID)
		m.ready.Store(true)
		m.cb.recordSuccess()
		attempt = 0
		m.transition(StateReady, "stream ready", nil)

		m.startSessionWorkers(sessionCtx, stream)

		err = m.waitSession(sessionCtx)
		m.endSession()
		m.ready.Store(false)
		m.sessionID.Store("")

		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				m.transition(StateShutdown, "session ended", err)
				return err
			}
			permanent, transient := classifyGRPCError(err)
			if permanent {
				m.transition(StateShutdown, "permanent session failure", err)
				m.shutdownErr = err
				return err
			}
			if transient {
				m.transition(StateTransientFailure, "session lost", err)
				attempt = m.handleTransientFailure(ctx, attempt, "session", err)
				continue
			}
			m.transition(StateTransientFailure, "session lost", err)
			attempt = m.handleTransientFailure(ctx, attempt, "session", err)
			continue
		}

		m.transition(StateTransientFailure, "session closed", nil)
	}
}

// Send implements Manager.
func (m *streamManager) Send(ctx context.Context, msg *agentv1.AgentMessage) error {
	if m == nil {
		return errors.New("stream manager is nil")
	}
	if msg == nil {
		return errors.New("message is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.IsThrottled() {
		if err := m.waitThrottle(ctx); err != nil {
			return err
		}
	}

	if m.ready.Load() {
		select {
		case m.sendCh <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			return ErrSendQueueFull
		}
	}

	return m.bufferMessage(ctx, msg)
}

// Connected implements Manager.
func (m *streamManager) Connected() bool {
	return m != nil && m.ready.Load()
}

// SessionID implements Manager.
func (m *streamManager) SessionID() string {
	if m == nil {
		return ""
	}
	v, _ := m.sessionID.Load().(string)
	return v
}

// IsThrottled implements Manager.
func (m *streamManager) IsThrottled() bool {
	if m == nil {
		return false
	}
	return m.throttle.throttled()
}

func (m *streamManager) waitThrottle(ctx context.Context) error {
	for m.throttle.throttled() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (m *streamManager) bufferMessage(ctx context.Context, msg *agentv1.AgentMessage) error {
	if msg.MessageId == "" {
		msg.MessageId = uuid.NewString()
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal agent message: %w", err)
	}
	item := queue.Item{
		ID:         msg.MessageId,
		Payload:    payload,
		EnqueuedAt: time.Now().UTC(),
	}
	if err := m.queue.Enqueue(ctx, item); err != nil {
		return fmt.Errorf("buffer message in queue: %w", err)
	}
	return nil
}

func (m *streamManager) currentState() ConnState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *streamManager) transition(next ConnState, reason string, err error) {
	m.mu.Lock()
	prev := m.state
	if prev == StateShutdown && next != StateShutdown {
		m.mu.Unlock()
		return
	}
	m.state = next
	if next == StateShutdown && err != nil {
		m.shutdownErr = err
	}
	m.mu.Unlock()

	fields := []logger.Field{
		logger.F("from", prev.String()),
		logger.F("to", next.String()),
		logger.F("reason", reason),
	}
	if err != nil {
		m.log.Err(err).Info("gRPC connection state transition", fields...)
	} else {
		m.log.Info("gRPC connection state transition", fields...)
	}
	m.metrics.setConnectionState(next)
}

func (m *streamManager) handleTransientFailure(ctx context.Context, attempt int, phase string, err error) int {
	n := m.cb.recordTransient()
	if n >= maxConsecutiveTransientFailures {
		m.log.Error("critical: consecutive transient gateway failures",
			logger.F("count", n),
			logger.F("phase", phase),
			logger.F("gateway", m.cfg.Gateway.Address),
			logger.F("cluster_id", m.cfg.Agent.ClusterID),
			logger.F("agent_id", m.cfg.Agent.AgentID),
			logger.F("error", err.Error()),
		)
	}
	m.transition(StateTransientFailure, phase+" failure", err)

	delay := backoff(attempt, m.cfg.Gateway.ReconnectInitialDelay, m.cfg.Gateway.ReconnectMaxDelay, m.rng)
	m.log.Warn("reconnecting after transient failure",
		logger.F("attempt", attempt),
		logger.F("delay", delay),
		logger.F("phase", phase),
	)
	if sleepErr := m.sleep(ctx, delay); sleepErr != nil {
		return attempt
	}
	return attempt + 1
}

func (m *streamManager) defaultDial(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
	creds, err := transportCredentials(&m.cfg.Gateway)
	if err != nil {
		return nil, nil, fmt.Errorf("transport credentials: %w", err)
	}

	ic := interceptorDeps{
		cfg:     func() *config.Config { return m.cfg },
		log:     m.log,
		metrics: m.metrics,
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithChainUnaryInterceptor(ic.chainUnary()...),
		grpc.WithChainStreamInterceptor(ic.chainStream()...),
	}

	conn, err := grpc.NewClient(m.cfg.Gateway.Address, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial gateway %q: %w", m.cfg.Gateway.Address, err)
	}

	return conn, agentv1.NewAgentServiceClient(conn), nil
}

func (m *streamManager) handshake(ctx context.Context, stream agentv1.AgentService_ConnectClient) (string, error) {
	req := &agentv1.AgentMessage{
		MessageId: uuid.NewString(),
		Payload: &agentv1.AgentMessage_Handshake{
			Handshake: &agentv1.HandshakeRequest{
				AgentVersion: AgentVersion,
				ProtoVersion: protoVersion,
				ClusterId:    m.cfg.Agent.ClusterID,
				TenantToken:  m.cfg.Agent.TenantToken,
				Caps: &agentv1.AgentCapabilities{
					Logs:    m.cfg.Collect.Logs.Enabled,
					State:   m.cfg.Collect.State.Enabled,
					Metrics: m.cfg.Collect.Metrics.Enabled,
				},
			},
		},
	}

	if err := stream.Send(req); err != nil {
		return "", fmt.Errorf("send handshake: %w", err)
	}

	timeout := m.cfg.Gateway.HandshakeTimeout
	if timeout <= 0 {
		timeout = handshakeMsgTimeout
	}
	recvCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type handshakeResult struct {
		resp *agentv1.GatewayMessage
		err  error
	}
	ch := make(chan handshakeResult, 1)
	go func() {
		resp, err := stream.Recv()
		ch <- handshakeResult{resp: resp, err: err}
	}()

	var result handshakeResult
	select {
	case <-recvCtx.Done():
		return "", fmt.Errorf("handshake response timeout: %w", recvCtx.Err())
	case result = <-ch:
	}

	if result.err != nil {
		return "", fmt.Errorf("receive handshake response: %w", result.err)
	}
	hs := result.resp.GetHandshake()
	if hs == nil {
		return "", errors.New("gateway response missing handshake payload")
	}
	if !hs.GetAccepted() {
		return "", handshakeRejected(hs.GetRejectionReason())
	}

	if hs.GetConfig() != nil {
		m.configSnap.Store(hs.GetConfig())
		m.log.Info("applied gateway config snapshot",
			logger.F("session_id", hs.GetSessionId()),
		)
	}

	return hs.GetSessionId(), nil
}

func (m *streamManager) startSessionWorkers(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	m.sessionWG.Add(3)
	go func() {
		defer m.sessionWG.Done()
		m.sendLoop(ctx, stream)
	}()
	go func() {
		defer m.sessionWG.Done()
		m.recvLoop(ctx, stream)
	}()
	go func() {
		defer m.sessionWG.Done()
		m.drainBufferedQueue(ctx, stream)
	}()
}

func (m *streamManager) waitSession(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.sessionWG.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (m *streamManager) endSession() {
	m.sessionMu.Lock()
	cancel := m.sessionCancel
	stream := m.stream
	conn := m.conn
	m.sessionCancel = nil
	m.stream = nil
	m.conn = nil
	m.sessionMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.CloseSend()
	}
	m.sessionWG.Wait()
	if conn != nil {
		_ = conn.Close()
	}
}

func (m *streamManager) sendLoop(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	for {
		if m.IsThrottled() {
			if err := m.waitThrottle(ctx); err != nil {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case msg := <-m.sendCh:
			if msg == nil {
				continue
			}
			if msg.MessageId == "" {
				msg.MessageId = uuid.NewString()
			}
			if err := stream.Send(msg); err != nil {
				m.log.Err(err).Warn("stream send failed")
				return
			}
		}
	}
}

func (m *streamManager) recvLoop(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	for {
		if ctx.Err() != nil {
			return
		}
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Err(err).Warn("stream recv ended")
			if m.metrics != nil {
				m.metrics.streamErrorsTotal.WithLabelValues("recv").Inc()
			}
			return
		}
		m.handleGatewayMessage(msg)
	}
}

func (m *streamManager) handleGatewayMessage(msg *agentv1.GatewayMessage) {
	if msg == nil {
		return
	}
	switch p := msg.Payload.(type) {
	case *agentv1.GatewayMessage_Backpressure:
		if p.Backpressure.GetThrottle() {
			delay := time.Duration(p.Backpressure.GetDelayMs()) * time.Millisecond
			m.throttle.pause(delay)
			m.log.Warn("gateway backpressure throttle",
				logger.F("delay", delay),
			)
		}
	case *agentv1.GatewayMessage_Config:
		m.log.Info("received gateway config update",
			logger.F("version", p.Config.GetConfigVersion()),
		)
	case *agentv1.GatewayMessage_Shutdown:
		m.log.Warn("gateway requested shutdown",
			logger.F("reason", p.Shutdown.GetReason()),
		)
	case *agentv1.GatewayMessage_Ack:
		// delivery acks handled by higher layers when wired
	default:
	}
}

func (m *streamManager) drainBufferedQueue(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	batchSize := m.cfg.Buffer.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	for ctx.Err() == nil && m.ready.Load() {
		items, err := m.queue.DequeueBatch(ctx, batchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Err(err).Warn("drain queue batch failed")
			return
		}
		if len(items) == 0 {
			continue
		}
		var ackIDs []string
		for _, item := range items {
			var msg agentv1.AgentMessage
			if err := proto.Unmarshal(item.Payload, &msg); err != nil {
				m.log.Err(err).Warn("skip invalid queued payload", logger.F("id", item.ID))
				ackIDs = append(ackIDs, item.ID)
				continue
			}
			if err := stream.Send(&msg); err != nil {
				_ = m.queue.Nack([]string{item.ID})
				m.log.Err(err).Warn("failed to send buffered message")
				return
			}
			ackIDs = append(ackIDs, item.ID)
		}
		if len(ackIDs) > 0 {
			_ = m.queue.Ack(ackIDs)
		}
	}
}

// ConfigSnapshot returns the latest config snapshot from the gateway, if any.
func (m *streamManager) ConfigSnapshot() *agentv1.ConfigSnapshot {
	if m == nil {
		return nil
	}
	return m.configSnap.Load()
}
