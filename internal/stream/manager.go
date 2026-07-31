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

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/pkg/buildinfo"
	"github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	"github.com/kubexa/kubexa-agent/pkg/protoversion"
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
				AgentVersion:            buildinfo.Version,
				ProtoVersion:            preferred,
				SupportedProtoVersions:  supported,
				ClusterId:               m.cfg.Agent.ClusterID,
				TenantToken:             m.cfg.Agent.TenantToken,
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
		return nil
	}
}

// abortSession cancels the active session context so all session workers unblock.
// It is safe to call multiple times and is invoked when any worker detects a
// broken stream (e.g. gateway restart) before endSession runs.
func (m *streamManager) abortSession() {
	m.sessionMu.Lock()
	cancel := m.sessionCancel
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
		m.reconcileWatchConfig(ctx, p.Config.GetConfig())
	case *agentv1.GatewayMessage_Shutdown:
		m.log.Warn("gateway requested shutdown",
			logger.F("reason", p.Shutdown.GetReason()),
		)
	case *agentv1.GatewayMessage_Ack:
		// delivery acks handled by higher layers when wired
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
			m.abortSession()
			return
		}
		if len(items) == 0 {
			continue
		}
		var ackIDs []string
		var skippedLogs int
		for _, item := range items {
			var msg agentv1.AgentMessage
			if err := proto.Unmarshal(item.Payload, &msg); err != nil {
				m.log.Err(err).Warn("skip invalid queued payload", logger.F("id", item.ID))
				ackIDs = append(ackIDs, item.ID)
				continue
			}
			if !shouldDeliverQueuedMessage(m.cfg, &msg) {
				skippedLogs++
				ackIDs = append(ackIDs, item.ID)
				continue
			}
			if err := stream.Send(&msg); err != nil {
				_ = m.queue.Nack([]string{item.ID})
				m.log.Err(err).Warn("failed to send buffered message")
				m.abortSession()
				return
			}
			ackIDs = append(ackIDs, item.ID)
		}
		if skippedLogs > 0 {
			m.log.Info("discarded buffered log messages (logs collection disabled)",
				logger.F("count", skippedLogs),
			)
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
