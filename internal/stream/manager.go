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
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/kubexa/kubexa-agent/internal/ingestrules"
	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/pkg/buildinfo"
	"github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	"github.com/kubexa/kubexa-agent/pkg/protoversion"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const (
	defaultSendChannelSize = 256
	handshakeMsgTimeout    = 10 * time.Second
)

// ErrSendQueueFull is returned when the internal send buffer is saturated.
var ErrSendQueueFull = errors.New("stream send queue full")

// WatchReconciler converges the agent's demand-driven informers on a desired
// set of Kubernetes resources. It is declared here, narrowed to just the one
// method this package needs, so that stream does not import the collector
// package. Satisfied by *state.Collector; the concrete type is wired in by
// cmd/agent/main.go.
type WatchReconciler interface {
	Reconcile(ctx context.Context, desired []k8sresource.Descriptor) error
}

// QueryResponder answers a live resource query from the gateway. It is
// declared here rather than imported so this package does not depend on
// internal/query; *query.Executor satisfies it structurally.
type QueryResponder interface {
	Execute(ctx context.Context, q *agentv1.ResourceQuery) *agentv1.ResourceQueryResult
}

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
	cfg           *config.Config
	queue         queue.Queue
	log           *logger.Logger
	streamMetrics *agentmetrics.StreamMetrics
	connMetrics   *agentmetrics.ConnectionMetrics

	cb    circuitBreaker
	rng   *rand.Rand
	sleep sleeper
	dial  dialFunc

	mu          sync.Mutex
	state       ConnState
	shutdownErr error
	sessionID   atomic.Value // string
	ready       atomic.Bool
	configSnap  atomic.Pointer[agentv1.ConfigSnapshot]
	// rules is the resolved ingest-rule set. The manager owns it because it is
	// the only component that sees the gateway's messages; the log collector
	// reads it on the collection path and drainBufferedQueue on the send path.
	rules *ingestrules.Store
	// counters are the cumulative pre-validation totals the heartbeat reports.
	// Shared with the log collector, which records truncation and rate limiting
	// while this side records age drops.
	counters *ingestrules.Counters

	// reconciler applies gateway watch config to the demand-driven informer
	// set. Set once at construction and read-only afterward, so it needs no
	// synchronization of its own. Nil is valid (e.g. state collection
	// disabled) — config updates are logged but no reconcile is attempted.
	reconciler WatchReconciler

	// responder answers live resource queries from the gateway. Set once at
	// construction and read-only afterward. Nil is valid — queries are
	// refused (handleResourceQuery is a no-op) when this agent does not run
	// live resource queries.
	responder QueryResponder

	sendCh   chan *agentv1.AgentMessage
	throttle throttleGate

	// active session (set only in StateReady, guarded by sessionMu)
	sessionMu sync.RWMutex
	stream    agentv1.AgentService_ConnectClient
	conn      *grpc.ClientConn

	// signals session goroutines to stop
	sessionCancel context.CancelFunc
	sessionWG     sync.WaitGroup
	// sessionErr is why a worker aborted the session, or nil for an ordinary
	// end. waitSession hands it to Run, which is the difference between
	// reconnecting immediately and reconnecting through the backoff ladder.
	// Guarded by sessionMu.
	sessionErr error

	// deliveryAcks records whether this gateway acks each agent message after
	// publishing it. False means the gateway is older than that feature, and
	// the agent must keep settling on Send -- an agent that waited for acks
	// that never come would hold every item to the deadline and then replay it
	// forever.
	deliveryAcks atomic.Bool
	// ackCh carries ack'd ids from recvLoop to the settle worker. recvLoop must
	// not call queue.Ack itself: that takes the queue's mutex and runs WAL
	// compaction, which would stall heartbeat and config handling behind disk
	// work.
	//
	// Created once here and outlives any one session: ackSettleLoop is started
	// fresh per session, so ids still sitting in ackCh when one session's loop
	// returns on cancel are picked up by the NEXT session's loop instead. By
	// then endSession has already nacked those same ids back onto the queue
	// (they were still inflight when the sweep ran), so the late ack applies
	// to a record the queue may already be treating as fresh again. That is
	// safe, not a bug: an ack only ever proves the earlier session's delivery
	// happened, and applying it late costs at most one harmless duplicate --
	// the price this whole design accepts on purpose. Do not switch to a
	// per-session channel to "fix" this; it would only turn a harmless late
	// apply into a lost one.
	ackCh chan []string
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

// New constructs a stream Manager wired to cfg, queue, logger, and shared
// agent metrics. reconciler receives every gateway watch config update; pass
// nil if this agent does not run demand-driven state collection. responder
// answers live resource queries; pass nil to refuse them.
func New(
	cfg *config.Config,
	q queue.Queue,
	log *logger.Logger,
	streamMetrics *agentmetrics.StreamMetrics,
	connMetrics *agentmetrics.ConnectionMetrics,
	reconciler WatchReconciler,
	responder QueryResponder,
	rules *ingestrules.Store,
	counters *ingestrules.Counters,
) (Manager, error) {
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

	m := &streamManager{
		cfg:           cfg,
		queue:         q,
		log:           log,
		streamMetrics: streamMetrics,
		connMetrics:   connMetrics,
		reconciler:    reconciler,
		responder:     responder,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec
		sleep:         defaultSleeper,
		sendCh:        make(chan *agentv1.AgentMessage, defaultSendChannelSize),
		state:         StateIdle,
		rules:         rules,
		counters:      counters,
		ackCh:         make(chan []string, 64),
	}
	// The manager is the store's only writer and Set panics on a nil receiver,
	// so a caller that passes none gets a private store rather than a crash on
	// the first handshake.
	if m.rules == nil {
		m.rules = ingestrules.NewStore()
	}
	m.sessionID.Store("")
	m.dial = m.defaultDial
	if connMetrics != nil {
		connMetrics.SetState(StateIdle.String())
	}
	return m, nil
}

// Run implements Manager.
func (m *streamManager) Run(ctx context.Context) error {
	if m == nil {
		return errors.New("stream manager is nil")
	}

	m.log.Info("stream manager starting")
	attempt := 0
	// stalled records that the last session died because the queue could not
	// settle its batch. It survives one iteration so the backoff ladder is not
	// reset by the handshake of a session that is about to fail the same way:
	// a ladder that restarts at its shortest rung on every attempt is not a
	// backoff, it is a fixed interval.
	stalled := false

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
		if !stalled {
			attempt = 0
		}
		stalled = false
		m.transition(StateReady, "stream ready", nil)

		m.startSessionWorkers(sessionCtx, stream)

		err = m.waitSession(ctx)
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
			stalled = errors.Is(err, errDrainStalled)
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
	if m.connMetrics != nil {
		m.connMetrics.SetState(next.String())
		// Counted here rather than at the dial site because this is the one
		// place that knows the previous state. StateConnecting is reached once
		// per pass of the run loop, and the loop only comes back around after a
		// session was lost -- so every entry but the first from StateIdle is a
		// reconnect. Without this the counter read 0 while the same process was
		// logging hundreds of recv errors, and the one metric that measures
		// reconnects was the one place a 150-second stream cap stayed invisible.
		if next == StateConnecting && prev != StateIdle {
			m.connMetrics.IncReconnects()
		}
	}
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
		cfg:           func() *config.Config { return m.cfg },
		log:           m.log,
		streamMetrics: m.streamMetrics,
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
	preferred, supported := protoversion.AgentHandshake()
	req := &agentv1.AgentMessage{
		MessageId: uuid.NewString(),
		Payload: &agentv1.AgentMessage_Handshake{
			Handshake: &agentv1.HandshakeRequest{
				AgentVersion:           buildinfo.Version,
				ProtoVersion:           preferred,
				SupportedProtoVersions: supported,
				ClusterId:              m.cfg.Agent.ClusterID,
				TenantToken:            m.cfg.Agent.TenantToken,
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
	if err := protoversion.ValidateGatewayResponse(hs.GetProtoVersion(), hs.GetSupportedProtoVersions()); err != nil {
		return "", handshakeRejected(err.Error())
	}
	if negotiated := protoversion.Normalize(hs.GetProtoVersion()); negotiated != "" {
		m.log.Info("proto version negotiated",
			logger.F("proto_version", negotiated),
		)
	}

	m.applyHandshakeConfig(hs)

	return hs.GetSessionId(), nil
}

// applyHandshakeConfig installs the seed configuration a handshake carries.
//
// The rules are set on EVERY handshake, including one carrying no config at
// all: FromProto(nil) is the agent's own defaults, so reconnecting to a
// gateway that states nothing restores them rather than leaving a previous
// session's rules in place. Rules that outlive the gateway that issued them
// would shape traffic against limits nobody currently claims.
//
// configSnap is only replaced when the handshake actually carries a snapshot,
// because a nil there means "no config", not "empty config".
func (m *streamManager) applyHandshakeConfig(hs *agentv1.HandshakeResponse) {
	// Stored on EVERY handshake, not just one carrying a config snapshot: an
	// agent upgraded ahead of its gateway must fall back to settling on Send,
	// and that fallback has to hold even when the handshake below has nothing
	// else to apply.
	m.deliveryAcks.Store(hs.GetDeliveryAcks())
	m.log.Info("gateway handshake accepted",
		logger.F("session_id", hs.GetSessionId()),
		logger.F("delivery_acks", hs.GetDeliveryAcks()),
	)
	m.rules.Set(ingestrules.FromProto(hs.GetConfig().GetIngestRules()))
	if hs.GetConfig() == nil {
		return
	}
	m.configSnap.Store(hs.GetConfig())
	m.log.Info("applied gateway config snapshot",
		logger.F("session_id", hs.GetSessionId()),
		logger.F("max_line_bytes", m.rules.Get().MaxLineBytes),
		logger.F("delivery_acks", hs.GetDeliveryAcks()),
	)
}

func (m *streamManager) startSessionWorkers(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	m.sessionWG.Add(5)
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
	go func() {
		defer m.sessionWG.Done()
		m.heartbeatLoop(ctx, stream)
	}()
	go func() {
		defer m.sessionWG.Done()
		m.ackSettleLoop(ctx)
	}()
}

// applyDeliveryAcks settles ids the gateway has confirmed. Idempotent over ids
// that are no longer inflight -- Ack skips them -- so a late ack for something
// the session-end sweep already returned is harmless: it costs one duplicate,
// which is the price this whole change is paying on purpose.
func (m *streamManager) applyDeliveryAcks(ids []string) {
	if len(ids) == 0 {
		return
	}
	if err := m.queue.Ack(ids); err != nil {
		if errors.Is(err, queue.ErrClosed) {
			return
		}
		m.log.Err(err).Warn("could not persist delivery acks",
			logger.F("count", len(ids)),
		)
		if m.streamMetrics != nil {
			m.streamMetrics.IncStreamError("delivery_ack")
		}
	}
}

// ackSettleLoop drains ackCh so recvLoop never blocks on the queue's mutex or
// on WAL compaction.
func (m *streamManager) ackSettleLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ids := <-m.ackCh:
			m.applyDeliveryAcks(ids)
		}
	}
}

// defaultHeartbeatInterval is used when the gateway states none. Thirty
// seconds is short enough that a stalled agent is visible within a scrape
// interval and long enough to cost nothing.
const defaultHeartbeatInterval = 30 * time.Second

// overloadedQueueFraction is how full the buffer has to be before the agent
// calls itself overloaded. It is a report, not a threshold anything acts on.
const overloadedQueueFraction = 0.8

// inflightAckDeadline bounds how long an item waits for a gateway ack that may
// never arrive. Past it the item is nacked: it is redelivered, and
// maxDeliveryAttempts still retires a genuinely poisonous one. Without this an
// ack lost to a cut would pin its WAL segment for the process's lifetime.
const inflightAckDeadline = 10 * time.Minute

// heartbeatLoop reports the agent's health on the interval the gateway asked
// for. Nothing sent one before this: agent.v1.Heartbeat has existed since the
// protocol was written and the gateway's handler only ever saw messages from
// the dev server.
//
// The counters it carries are CUMULATIVE since process start. A dropped
// heartbeat then loses nothing, and the gateway advances its own Prometheus
// counters by the difference.
func (m *streamManager) heartbeatLoop(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	interval := defaultHeartbeatInterval
	if sec := m.ConfigSnapshot().GetHeartbeatIntervalSec(); sec > 0 {
		interval = time.Duration(sec) * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			msg := &agentv1.AgentMessage{
				MessageId: uuid.NewString(),
				Payload: &agentv1.AgentMessage_Heartbeat{
					Heartbeat: &agentv1.Heartbeat{
						TimestampUnixMs: time.Now().UnixMilli(),
						Health:          m.healthSnapshot(),
					},
				},
			}
			// Sent straight down the stream rather than through the queue: a
			// health report that waits behind the backlog it is reporting on
			// is worthless, and it must not be replayed from disk after a
			// restart either.
			if err := stream.Send(msg); err != nil {
				m.log.Err(err).Warn("heartbeat send failed")
				m.abortSession()
				return
			}
			m.sweepStaleInflight()
		}
	}
}

// sweepStaleInflight returns items whose ack never arrived and reports the
// oldest remaining age. Called from the periodic loop.
func (m *streamManager) sweepStaleInflight() {
	if !m.deliveryAcks.Load() {
		return
	}
	if n, err := m.queue.NackInflightOlderThan(inflightAckDeadline); err != nil {
		if !errors.Is(err, queue.ErrClosed) {
			m.log.Err(err).Warn("stale inflight sweep failed", logger.F("count", n))
		}
	} else if n > 0 {
		m.log.Warn("returned items whose delivery ack never arrived",
			logger.F("count", n),
			logger.F("deadline", inflightAckDeadline.String()),
		)
	}
}

// healthSnapshot reads the queue and the process-lifetime counters the
// pre-validation paths maintain.
func (m *streamManager) healthSnapshot() *agentv1.AgentHealth {
	var depth, dropped int64
	if m.queue != nil {
		depth = m.queue.Depth()
		dropped = m.queue.DroppedTotal()
	}
	truncated, tooOld, future, rateLimited := m.counters.Snapshot()
	return &agentv1.AgentHealth{
		QueueDepth:         depth,
		DroppedMessages:    dropped,
		Status:             m.healthStatus(depth),
		TruncatedLines:     truncated,
		DroppedTooOld:      tooOld,
		DroppedFuture:      future,
		DroppedRateLimited: rateLimited,
	}
}

// healthStatus is a coarse self-report, not a measurement of anything the
// agent acts on. It says "overloaded" only when the buffer is genuinely close
// to full -- which needs a queue that knows its own capacity -- and "degraded"
// while the gateway has it throttled.
func (m *streamManager) healthStatus(depth int64) string {
	if ca, ok := m.queue.(queue.CapacityAware); ok {
		if capacity := ca.Capacity(); capacity > 0 &&
			float64(depth) >= overloadedQueueFraction*float64(capacity) {
			return "overloaded"
		}
	}
	if m.IsThrottled() {
		return "degraded"
	}
	return "healthy"
}

// waitSession blocks until all session workers exit or the process context is cancelled.
// Session workers may exit early when abortSession cancels the session context; that
// is not a process shutdown and returns nil so Run can reconnect.
func (m *streamManager) waitSession(parentCtx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.sessionWG.Wait()
		close(done)
	}()

	select {
	case <-parentCtx.Done():
		return parentCtx.Err()
	case <-done:
		m.sessionMu.RLock()
		defer m.sessionMu.RUnlock()
		return m.sessionErr
	}
}

// abortSession cancels the active session context so all session workers unblock.
// It is safe to call multiple times and is invoked when any worker detects a
// broken stream (e.g. gateway restart) before endSession runs.
//
// The session ends with no error, so Run reconnects at once. That is right for
// a broken stream -- the dial that follows will report the real state of the
// link -- and wrong for a failure the reconnect cannot fix, which is what
// abortSessionErr is for.
func (m *streamManager) abortSession() {
	m.abortSessionErr(nil)
}

// abortSessionErr ends the session and records why.
//
// A non-nil reason travels back through waitSession, so Run takes its
// transient-failure path and sleeps the backoff before reconnecting instead of
// looping at the speed of a dial. Without it a worker that keeps failing for a
// reason no reconnect can fix -- a full disk refusing every ack -- rebuilt the
// session tens of times a second: a handshake storm at the gateway, and
// sendLoop and the heartbeat interrupted with it.
//
// The first reason wins: later aborts are usually the same failure arriving
// through the other workers as a cancelled context.
func (m *streamManager) abortSessionErr(reason error) {
	m.sessionMu.Lock()
	cancel := m.sessionCancel
	if reason != nil && m.sessionErr == nil {
		m.sessionErr = reason
	}
	m.sessionMu.Unlock()
	if cancel != nil {
		cancel()
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
	// Cleared here, after waitSession has already read it: the reason belongs
	// to the session that is ending, and carrying it into the next one would
	// back off against a failure that is over.
	m.sessionErr = nil
	m.sessionMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.CloseSend()
	}
	m.sessionWG.Wait()

	// Every item the gateway never acked comes back. This runs after
	// sessionWG.Wait(), so the drain goroutine has exited and nothing else is
	// touching inflight.
	//
	// Only under delivery acks: without them the drain loop already settled
	// every item before returning, and sweeping would find nothing.
	if m.deliveryAcks.Load() {
		if n, err := m.queue.NackInflight(); err != nil {
			if !errors.Is(err, queue.ErrClosed) {
				m.log.Err(err).Warn("could not return unacked items at session end",
					logger.F("count", n),
				)
			}
		} else if n > 0 {
			m.log.Info("returned unacked items to the queue at session end",
				logger.F("count", n),
			)
		}
	}

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
				m.abortSession()
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
			if m.streamMetrics != nil {
				m.streamMetrics.IncStreamError("recv")
			}
			m.abortSession()
			return
		}
		m.handleGatewayMessage(ctx, msg)
	}
}

func (m *streamManager) handleGatewayMessage(ctx context.Context, msg *agentv1.GatewayMessage) {
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
		// ConfigSnapshot's message-typed fields are a PATCH: nil means
		// unchanged. Replacing the stored snapshot wholesale would let a
		// watchers-only update from the gateway's clusterwatch reconciler
		// erase the rules, and a rules-only update erase the watchers.
		if rules := p.Config.GetConfig().GetIngestRules(); rules != nil {
			m.rules.Set(ingestrules.FromProto(rules))
			m.log.Info("applied pushed ingest rules",
				logger.F("max_line_bytes", rules.GetMaxLineBytes()),
				logger.F("per_stream_rate_bytes", rules.GetPerStreamRateBytes()),
			)
		}
		// A watchers-less update is not "stop watching everything" -- it is an
		// update about something else. This test is exact, not a heuristic:
		// kubexa-backend's clusterwatch reconciler always sends exactly one
		// WatcherConfig entry (internal/clusterwatch/reconciler.go, syncGroup),
		// carrying an empty Resources list when the desired set is empty. So
		// "tear everything down" arrives as one entry with no resources, never
		// as zero entries.
		if len(p.Config.GetConfig().GetWatchers()) > 0 {
			m.reconcileWatchConfig(ctx, p.Config.GetConfig())
		}
	case *agentv1.GatewayMessage_Shutdown:
		m.log.Warn("gateway requested shutdown",
			logger.F("reason", p.Shutdown.GetReason()),
		)
	case *agentv1.GatewayMessage_Ack:
		// Gated so an old-mode agent (no delivery_acks) never takes the queue
		// mutex or runs WAL compaction for an ack it doesn't need: the false
		// path must be byte-for-byte today's behaviour, and a gateway that
		// predates this feature never sends an Ack anyway.
		if m.deliveryAcks.Load() {
			ids := p.Ack.GetMessageIds()
			if len(ids) == 0 {
				break
			}
			select {
			case m.ackCh <- ids:
			default:
				// The settle worker is behind. Dropping the ack is safe and the
				// only non-blocking option: the items stay inflight, the session-end
				// sweep returns them, and the gateway gets them again. A blocking
				// send here would stall recvLoop behind disk work.
				m.log.Warn("delivery ack settle queue full; these items will be redelivered",
					logger.F("count", len(ids)),
				)
			}
		}
	case *agentv1.GatewayMessage_ResourceQuery:
		m.handleResourceQuery(ctx, p.ResourceQuery)
	default:
	}
}

// handleResourceQuery answers a live query on its own goroutine.
//
// Running it inline would block the recv loop -- and therefore acks,
// backpressure and shutdown -- for as long as the query takes, which the
// executor allows to be 30 seconds. The executor's own concurrency gate is
// what bounds how many of these goroutines can be doing real work at once.
//
// This goroutine carries the session ctx but is deliberately not registered
// with sessionWG, so endSession's sessionWG.Wait() does not wait for it to
// finish. A query reply is session-scoped: once the session ends the
// gateway's stream is gone, so a result that arrives after reconnect has
// nowhere useful to go. Draining it on shutdown would only delay teardown
// for no benefit; ctx cancellation (and Send's own failure once the stream
// is torn down) is what stops it from doing pointless work.
func (m *streamManager) handleResourceQuery(ctx context.Context, q *agentv1.ResourceQuery) {
	if m.responder == nil || q == nil {
		return
	}
	go func() {
		result := m.responder.Execute(ctx, q)
		if result == nil {
			return
		}
		msg := &agentv1.AgentMessage{
			MessageId: uuid.NewString(),
			Payload:   &agentv1.AgentMessage_ResourceQueryResult{ResourceQueryResult: result},
		}
		if err := m.Send(ctx, msg); err != nil {
			m.log.Err(err).Warn("failed to send resource query result",
				logger.F("query_id", q.GetQueryId()),
			)
		}
	}()
}

// reconcileWatchConfig maps every WatcherConfig.Resources entry across every
// watcher in snapshot into a Descriptor and reconciles the demand-driven
// informer set on their union in a single call. Reconciling per watcher
// instead would have each call stop what the previous one just started,
// since Reconcile always converges the whole set to exactly what it is
// given — it has no notion of "add to" or "remove from".
//
// A snapshot with no watchers (or watchers with no resources) is not treated
// as "ignore this message": it is reconciled to an empty desired set like
// any other, which is what actually stops informers once the last viewer's
// tab closes. Skipping it here would leak every demand-driven watch forever.
//
// ctx is whatever the caller (the gateway recv loop) naturally has for this
// message; it scopes the reconcile call only. The reconciler is responsible
// for parenting any informers it starts on its own lifetime, not on ctx.
func (m *streamManager) reconcileWatchConfig(ctx context.Context, snapshot *agentv1.ConfigSnapshot) {
	if m.reconciler == nil {
		return
	}

	var desired []k8sresource.Descriptor
	for _, watcher := range snapshot.GetWatchers() {
		for _, ref := range watcher.GetResources() {
			name := resourceRefName(ref)
			desc, err := k8sresource.Parse(name)
			// resourceRefName always contains a "/" (even for a ref with
			// every field blank, it joins down to "/"), so Parse's own
			// no-slash and empty-name error paths can never trigger here —
			// every ResourceRef routes into parseGVR. A ref missing Version
			// or Resource still returns a nil error there: descriptorForGVR
			// answers a blank GVR with a zero-value Descriptor instead of an
			// error. Treat that zero value (Resource == "") as unparsable
			// too, or a malformed ref would silently reach Reconcile as a
			// no-op descriptor instead of being logged and skipped.
			if err != nil || desc.GVR.Resource == "" {
				m.log.Err(err).Warn("skip unparsable watch resource",
					logger.F("resource", name),
				)
				continue
			}
			desired = append(desired, desc)
		}
	}

	if err := m.reconciler.Reconcile(ctx, desired); err != nil {
		m.log.Err(err).Warn("reconcile watch config failed")
	}
}

// resourceRefName renders a ResourceRef the way k8sresource.Parse expects:
// "group/version/resource", or bare "version/resource" for the core group.
// Joining unconditionally would put a leading "/" on every core-group ref
// (e.g. "/v1/pods"); parseGVR happens to parse that the same as "v1/pods"
// today (an empty first segment still yields Group ""), but nothing pins
// that equivalence, so this builds the canonical two-part form directly
// instead of relying on it.
func resourceRefName(ref *agentv1.ResourceRef) string {
	group := ref.GetGroup()
	version := ref.GetVersion()
	resource := ref.GetResource()
	if group == "" {
		return version + "/" + resource
	}
	return group + "/" + version + "/" + resource
}

// shouldDeliverQueuedMessage reports whether a recovered queue item may be sent on the stream.
// Log payloads are dropped when collect.logs.enabled is false so stale spill data is not exported.
func shouldDeliverQueuedMessage(cfg *config.Config, msg *agentv1.AgentMessage) bool {
	if cfg == nil || msg == nil {
		return false
	}
	if _, isLog := msg.Payload.(*agentv1.AgentMessage_Logs); isLog && !cfg.Collect.Logs.Enabled {
		return false
	}
	return true
}

// ageDropReason reports why a queued log batch must not be sent, or "" to send
// it. Only log batches are checked: state events and metrics have their own
// freshness semantics and Loki's reject_old_samples does not apply to them.
//
// The check lives at drain, not at collection, because age is a SEND-time
// property: a record queued during a three-day outage was fresh when it was
// collected. Loki would reject these anyway -- dropping them here saves the
// bandwidth and, unlike a rejection, produces a number the agent can report.
//
// A batch is judged by its OLDEST entry, and dropped whole. The collection
// path writes one entry per message, so in practice that is the same thing;
// judging by the newest would let one fresh entry carry a week of stale ones
// past the gate.
func ageDropReason(msg *agentv1.AgentMessage, r ingestrules.Rules, now time.Time) string {
	if r.MaxSampleAge <= 0 && r.MaxFutureSkew <= 0 {
		return ""
	}
	logs := msg.GetLogs()
	if logs == nil || len(logs.GetEntries()) == 0 {
		return ""
	}
	for _, e := range logs.GetEntries() {
		ts := time.Unix(0, e.GetTimestamp())
		if r.MaxSampleAge > 0 && now.Sub(ts) > r.MaxSampleAge {
			return "too_old"
		}
		if r.MaxFutureSkew > 0 && ts.Sub(now) > r.MaxFutureSkew {
			return "future"
		}
	}
	return ""
}

// maxInflightBatches caps how many dequeued batches may await a gateway ack.
//
// Before delivery acks, drainBufferedQueue settled every item before dequeuing
// again, so inflight was at most one batch and the queue's own memory
// accounting covered everything. Waiting for a remote ack removed that bound: a
// gateway that is up but not acking -- its own buffer full, or NATS down -- lets
// this loop pull the entire disk queue into memory, and a memory-tier inflight
// entry still holds its payload after memBytes was decremented, so nothing
// bounds the bytes. That is the OOM this project spent two releases escaping.
//
// Two rather than one keeps the pipeline full -- one batch on the wire while
// the next is prepared -- at twice the pre-ack envelope. Headroom: at
// batch_size 100 and the gateway's 200 ms ack flush this still clears roughly
// 17k items/min, against a measured production rate near 1,068/min.
const maxInflightBatches = 2

// inflightCounter is how the drain loop reads the queue's inflight count. It is
// deliberately not on the Queue interface -- the same observation-hook standing
// as InflightLen itself -- so the manager asserts for it and simply does not
// bound when an implementation does not offer it.
type inflightCounter interface {
	InflightLen() int
}

// waitForInflightRoom blocks until a full batch of batchSize items would fit
// within maxInflightBatches batches worth of inflight capacity, or the
// session ends. A non-nil return means the caller must leave the drain loop.
//
// The condition is InflightLen()+batchSize > limit, not InflightLen() >=
// limit: waiting only until there is room for *something* still lets the next
// DequeueBatch add up to batchSize-1 more than the envelope promises, peaking
// at batchSize*(maxInflightBatches+1)-1 -- 299 at the default batch_size 100 --
// instead of the batchSize*maxInflightBatches this function exists to enforce.
func (m *streamManager) waitForInflightRoom(ctx context.Context, counter inflightCounter, batchSize int) error {
	limit := batchSize * maxInflightBatches
	if limit <= 0 {
		return nil
	}
	for counter.InflightLen()+batchSize > limit {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if !m.ready.Load() {
			return errors.New("session ended while waiting for inflight room")
		}
	}
	return nil
}

func (m *streamManager) drainBufferedQueue(ctx context.Context, stream agentv1.AgentService_ConnectClient) {
	batchSize := m.cfg.Buffer.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	counter, bounded := m.queue.(inflightCounter)
	for ctx.Err() == nil && m.ready.Load() {
		if bounded && m.deliveryAcks.Load() {
			if err := m.waitForInflightRoom(ctx, counter, batchSize); err != nil {
				return
			}
		}
		items, err := m.queue.DequeueBatch(ctx, batchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Err(err).Warn("drain queue batch failed")
			m.abortSession()
			return
		}
		if len(items) == 0 {
			continue
		}
		// Two disjoint sets, because they are settled differently when the ack
		// cannot be persisted. sentIDs were delivered and may be redelivered;
		// terminalIDs were judged undeliverable here -- unparseable, disabled,
		// too old -- and must never go back on the queue, which is exactly why
		// they are acked rather than nacked in the normal path.
		var sentIDs []string
		var terminalIDs []string
		var skippedLogs int
		for idx, item := range items {
			var msg agentv1.AgentMessage
			if err := proto.Unmarshal(item.Payload, &msg); err != nil {
				m.log.Err(err).Warn("skip invalid queued payload", logger.F("id", item.ID))
				terminalIDs = append(terminalIDs, item.ID)
				continue
			}
			if !shouldDeliverQueuedMessage(m.cfg, &msg) {
				skippedLogs++
				terminalIDs = append(terminalIDs, item.ID)
				continue
			}
			if reason := ageDropReason(&msg, m.rules.Get(), time.Now()); reason != "" {
				m.countAgeDrop(reason)
				// Acked, not nacked: the record is not retryable. It will only
				// get older, and a nack would replay it forever.
				terminalIDs = append(terminalIDs, item.ID)
				continue
			}
			if err := stream.Send(&msg); err != nil {
				// The failed item AND everything after it are still ours: they
				// were dequeued into the inflight map and nothing else will
				// ever return them. Returning here without nacking stranded
				// them until the process restarted, and with WAL compaction a
				// stranded item pins its segment forever.
				//
				// They are handed back differently, though: Send was called
				// for this one item only, so it is the only one that has used
				// a delivery attempt. Charging the untried remainder let one
				// poison item at the head retire the whole batch at the cap --
				// measured 10 items dropped for 1 undeliverable one, up to
				// buffer.batch_size (default 100) in production.
				untriedIDs := make([]string, 0, len(items)-idx-1)
				for _, remaining := range items[idx+1:] {
					untriedIDs = append(untriedIDs, remaining.ID)
				}
				// Untried first, then the failure: each nack prepends to the
				// front of the queue, so the LAST call ends up ahead. That
				// restores the batch's own order, with the failed item back at
				// the head. This drain goroutine is the queue's only consumer
				// and it is about to end the session, so nothing dequeues
				// between the two calls.
				m.settleUntriedNacks(untriedIDs)
				// Nack before ack so no ID is handled twice: the sets are
				// disjoint, and this ordering keeps them so under any future
				// edit.
				m.settleNacks([]string{item.ID})
				// Same rule as the batch-level settle: under delivery acks the
				// already-sent items are unproven and belong to the session-end
				// sweep, not to an ack here.
				settleSent := sentIDs
				if m.deliveryAcks.Load() {
					settleSent = nil
				}
				// The session is ending on the send error either way, so the
				// settle result only matters for what it did, not what it
				// reports: it has already returned or dropped everything.
				_ = m.settleAcks(settleSent, terminalIDs)
				m.log.Err(err).Warn("failed to send buffered message")
				m.abortSession()
				return
			}
			sentIDs = append(sentIDs, item.ID)
		}
		if skippedLogs > 0 {
			m.log.Info("discarded buffered log messages (logs collection disabled)",
				logger.F("count", skippedLogs),
			)
		}
		// Under delivery acks, sentIDs are NOT settled here: Send returning nil
		// only means the message entered a send buffer. They stay inflight
		// until the gateway's Ack arrives, or until the session-end sweep or
		// the deadline sweep returns them.
		//
		// terminalIDs are settled either way. Those never went near the wire --
		// unparseable, disabled, too old -- and the agent judged them
		// undeliverable itself, so acking them locally is correct.
		settleSent := sentIDs
		if m.deliveryAcks.Load() {
			settleSent = nil
		}
		if err := m.settleAcks(settleSent, terminalIDs); err != nil {
			if errors.Is(err, queue.ErrClosed) {
				// Shutdown. Nothing to back off from and nothing left to
				// drain; the WAL replays whatever is still inflight.
				return
			}
			// The acks could not be persisted, so the batch went back on the
			// queue. Looping would dequeue and re-send the very same items at
			// full speed -- measured at ~100k re-sends in 200ms, which is
			// duplicate traffic to the gateway plus an agent log flooding the
			// same pipeline as its own diagnostics.
			//
			// The session ends rather than the worker simply returning,
			// because this is a per-session worker: nothing re-enters it, so a
			// bare return would leave the queue undrained for the life of a
			// session that might last hours. It ends WITH a reason, so Run
			// takes its transient-failure path and sleeps the backoff ladder
			// first -- an unadorned abort ends the session with no error at
			// all, and Run reconnects at the speed of a dial: ~35 sessions a
			// second, measured. No sleep is added here and no queue lock is
			// held across the wait.
			m.abortSessionErr(fmt.Errorf("%w: %v", errDrainStalled, err))
			return
		}
	}
}

// maxAckAttempts bounds the retry of a failed Ack.
//
// A second attempt is worth making because Ack compacts before it returns even
// when it failed, so the disk that had no room for the ack record may have room
// by the time the call comes back. A third would only spin: the retry is
// immediate and takes the queue lock each time, and everything that could have
// changed in between has already changed.
const maxAckAttempts = 2

// errDrainStalled marks a session that ended because the queue could not
// settle its batch. Run backs off on it rather than reconnecting at once: no
// dial fixes a full disk, and the reconnect is not the retry -- the next
// drain is.
var errDrainStalled = errors.New("buffered queue could not settle its batch")

// settleAcks acks both id sets and guarantees that none of them is left in the
// queue's inflight map afterwards. It returns nil once the acks are persisted;
// any error means the batch was settled the hard way and the caller must not
// dequeue again straight away.
//
// Ack deliberately KEEPS the inflight entry when the ack could not be
// persisted: the entry is what justifies holding the WAL segment claim, and
// releasing it would let an ack that was never written be undone by a restart.
// That is right at the queue layer, but it makes the entry releasable only by a
// later Ack, Nack or Drop -- so a caller that discards the error strands the
// entry, its payload and its segment for the lifetime of the process.
//
// After the bounded retry the two sets part ways: sent ids go back on the queue
// through NackDelivered, which redelivers them exactly as Nack does but records
// a later retirement as delivered-but-unrecorded rather than as data loss --
// the gateway has that data. Terminal ids are dropped, because requeueing
// something already judged undeliverable only replays it until it is judged
// undeliverable again.
func (m *streamManager) settleAcks(sentIDs, terminalIDs []string) error {
	ids := make([]string, 0, len(sentIDs)+len(terminalIDs))
	ids = append(ids, sentIDs...)
	ids = append(ids, terminalIDs...)
	if len(ids) == 0 {
		return nil
	}

	var err error
	for attempt := 0; attempt < maxAckAttempts; attempt++ {
		// Ack is idempotent over ids it has already retired -- it skips any id
		// no longer inflight -- so a retry of the whole slice only re-attempts
		// the tail the failed call stopped at.
		err = m.queue.Ack(ids)
		if err == nil {
			return nil
		}
		if errors.Is(err, queue.ErrClosed) {
			// Shutdown, not a fault. Nothing can be settled on a closed queue
			// -- Nack and Drop refuse it too -- and the WAL replays whatever
			// is left on the next start. Retrying or warning here only adds
			// noise to every drain worker on the way out.
			return err
		}
	}

	m.log.Err(err).Warn("could not persist acks, settling the batch by hand",
		logger.F("sent", len(sentIDs)),
		logger.F("terminal", len(terminalIDs)),
	)
	if m.streamMetrics != nil {
		m.streamMetrics.IncStreamError("queue_ack")
	}
	m.settleDeliveredNacks(sentIDs)
	m.settleDrops(terminalIDs)
	return err
}

// settleDeliveredNacks returns ids that reached the gateway but whose ack could
// not be recorded. Same redelivery as settleNacks; the difference is that the
// queue counts a retirement of one of these as delivered-but-unrecorded rather
// than as a drop, so dropped_total keeps meaning "telemetry the agent lost".
func (m *streamManager) settleDeliveredNacks(ids []string) {
	if len(ids) == 0 {
		return
	}
	m.reportSettleFailure(m.queue.NackDelivered(ids), "could not requeue delivered messages", "queue_nack", len(ids))
}

// settleNacks returns ids to the queue for redelivery.
//
// Nack settles every id it is given -- requeued, retired at the attempt cap, or
// counted as dropped when there is nowhere to put it -- so an error here says
// some of them were lost, not that they are still in flight. The exception is a
// closed queue, which refuses the call before touching anything: those ids do
// stay in the map, and that is fine, because the process is on its way out and
// the WAL replays them.
func (m *streamManager) settleNacks(ids []string) {
	if len(ids) == 0 {
		return
	}
	m.reportSettleFailure(m.queue.Nack(ids), "could not requeue buffered messages", "queue_nack", len(ids))
}

// settleUntriedNacks returns ids that were dequeued but never sent: the batch
// queued behind the item whose send failed. Same redelivery as settleNacks, and
// the same guarantee that every id is settled; the difference is that these are
// charged no delivery attempt, because none was made on them.
func (m *streamManager) settleUntriedNacks(ids []string) {
	if len(ids) == 0 {
		return
	}
	m.reportSettleFailure(m.queue.NackUntried(ids), "could not requeue untried messages", "queue_nack", len(ids))
}

// settleDrops releases ids that must never be retried. Same closed-queue
// exemption as settleNacks, for the same reason.
func (m *streamManager) settleDrops(ids []string) {
	if len(ids) == 0 {
		return
	}
	m.reportSettleFailure(m.queue.Drop(ids), "could not drop undeliverable buffered messages", "queue_drop", len(ids))
}

// reportSettleFailure logs and counts a settle call that failed, treating a
// closed queue as the shutdown it is rather than as a fault.
func (m *streamManager) reportSettleFailure(err error, msg, reason string, count int) {
	if err == nil || errors.Is(err, queue.ErrClosed) {
		return
	}
	m.log.Err(err).Warn(msg, logger.F("count", count))
	if m.streamMetrics != nil {
		m.streamMetrics.IncStreamError(reason)
	}
}

// countAgeDrop records one age-filtered record under its reason, on both the
// agent's own metrics and the cumulative counters the heartbeat reports.
func (m *streamManager) countAgeDrop(reason string) {
	switch reason {
	case "too_old":
		m.counters.IncTooOld()
	case "future":
		m.counters.IncFuture()
	}
	if m.streamMetrics != nil {
		m.streamMetrics.IncStreamError("age_" + reason)
	}
}

// ConfigSnapshot returns the latest config snapshot from the gateway, if any.
func (m *streamManager) ConfigSnapshot() *agentv1.ConfigSnapshot {
	if m == nil {
		return nil
	}
	return m.configSnap.Load()
}
