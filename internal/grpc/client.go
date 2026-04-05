package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/kubexa/kubexa-agent/gen/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/config"
)

// ─────────────────────────────────────────
// State
// ─────────────────────────────────────────

type ConnectionState int32

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateClosed
)

func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ─────────────────────────────────────────
// Client
// ─────────────────────────────────────────

// Client gRPC bağlantısını ve AgentService stub'ını yönetir.
type Client struct {
	cfg        config.GRPCConfig
	backendCfg config.BackendConfig
	clusterID  string

	mu    sync.RWMutex
	conn  *grpc.ClientConn
	stub  agentv1.AgentServiceClient
	state atomic.Int32

	reconnector *Reconnector
}

func NewClient(cfg config.GRPCConfig, backendCfg config.BackendConfig, clusterID string) *Client {
	c := &Client{
		cfg:        cfg,
		backendCfg: backendCfg,
		clusterID:  clusterID,
	}
	c.state.Store(int32(StateDisconnected))
	return c
}

// ─────────────────────────────────────────
// Connect
// ─────────────────────────────────────────

// Connect backend'e bağlanır.
// TLS config'e göre otomatik seçilir.
func (c *Client) Connect(ctx context.Context) error {
	c.state.Store(int32(StateConnecting))

	conn, err := c.dial(ctx)
	if err != nil {
		c.state.Store(int32(StateDisconnected))
		return fmt.Errorf("gRPC bağlantısı kurulamadı: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.stub = agentv1.NewAgentServiceClient(conn)
	c.mu.Unlock()

	c.state.Store(int32(StateConnected))

	log.Info().
		Str("address", c.backendCfg.Address()).
		Bool("tls", c.backendCfg.TLS).
		Msg("gRPC bağlantısı kuruldu")

	// bağlantı durumunu izle
	go c.watchConnectivity(ctx)

	return nil
}

// ─────────────────────────────────────────
// Dial
// ─────────────────────────────────────────

func (c *Client) dial(ctx context.Context) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		// Keepalive Time must stay above the server's MinPingInterval (often 5m) or the server
		// may reply with GOAWAY ENHANCE_YOUR_CALM "too_many_pings". PermitWithoutStream avoids
		// extra pings when no RPC is active (common with strict LBs / Apigee / Cloud Run, etc.).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                c.cfg.KeepaliveTime,
			Timeout:             c.cfg.KeepaliveTimeout,
			PermitWithoutStream: false,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16*1024*1024), // 16MB
			grpc.MaxCallSendMsgSize(16*1024*1024), // 16MB
		),
		grpc.WithChainUnaryInterceptor(
			c.authInterceptor(),
			c.loggingUnaryInterceptor(),
		),
		grpc.WithChainStreamInterceptor(
			c.authStreamInterceptor(),
			c.loggingStreamInterceptor(),
		),
	}

	// TLS
	if c.backendCfg.TLS {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		opts = append(opts, grpc.WithTransportCredentials(
			credentials.NewTLS(tlsCfg),
		))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		))
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()

	return grpc.DialContext(dialCtx, c.backendCfg.Address(), opts...) //nolint:staticcheck
}

// ─────────────────────────────────────────
// Connectivity Watch
// ─────────────────────────────────────────

// watchConnectivity bağlantı durumunu izler.
// bağlantı koparsa Reconnector'ı tetikler.
func (c *Client) watchConnectivity(ctx context.Context) {
	for {
		c.mu.RLock()
		conn := c.conn
		c.mu.RUnlock()

		if conn == nil {
			return
		}

		state := conn.GetState()

		// bağlantı koptu mu?
		if state == connectivity.TransientFailure ||
			state == connectivity.Shutdown {

			if c.State() == StateClosed {
				return
			}

			log.Warn().
				Str("grpc_state", state.String()).
				Msg("gRPC bağlantısı koptu, reconnect başlatılıyor")

			c.state.Store(int32(StateDisconnected))

			if c.reconnector != nil {
				c.reconnector.Trigger()
			}
			return
		}

		// durum değişikliği bekle
		if !conn.WaitForStateChange(ctx, state) {
			return // context iptal
		}
	}
}

// ─────────────────────────────────────────
// Stub
// ─────────────────────────────────────────

// Stub AgentService client stub'ını döner.
// Bağlantı yoksa hata döner.
func (c *Client) Stub() (agentv1.AgentServiceClient, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.stub == nil {
		return nil, fmt.Errorf("gRPC stub hazır değil, bağlantı durumu: %s", c.State())
	}
	return c.stub, nil
}

// SetReconnector reconnector'ı set eder.
func (c *Client) SetReconnector(r *Reconnector) {
	c.reconnector = r
}

// ─────────────────────────────────────────
// State
// ─────────────────────────────────────────

func (c *Client) State() ConnectionState {
	return ConnectionState(c.state.Load())
}

func (c *Client) IsConnected() bool {
	return c.State() == StateConnected
}

// ─────────────────────────────────────────
// Close
// ─────────────────────────────────────────

func (c *Client) Close() error {
	c.state.Store(int32(StateClosed))

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			return fmt.Errorf("gRPC bağlantısı kapatılamadı: %w", err)
		}
		c.conn = nil
		c.stub = nil
	}

	log.Info().Msg("gRPC client kapatıldı")
	return nil
}

// ─────────────────────────────────────────
// Interceptors
// ─────────────────────────────────────────

func (c *Client) authInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx = c.attachToken(ctx)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func (c *Client) authStreamInterceptor() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		ctx = c.attachToken(ctx)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func (c *Client) loggingUnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		log.Debug().
			Str("method", method).
			Dur("duration", time.Since(start)).
			Err(err).
			Msg("gRPC unary call")
		return err
	}
}

func (c *Client) loggingStreamInterceptor() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		log.Debug().
			Str("method", method).
			Msg("gRPC stream açıldı")
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// attachToken bearer token ve cluster id'yi context metadata'sına ekler.
func (c *Client) attachToken(ctx context.Context) context.Context {
	ctx = metadata.AppendToOutgoingContext(ctx,
		"x-cluster-id", c.clusterID,
	)
	if c.backendCfg.Token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"authorization", "Bearer "+c.backendCfg.Token,
	)
}
