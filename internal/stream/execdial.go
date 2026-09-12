package stream

import (
	"context"
	"sync"

	"google.golang.org/grpc"

	"github.com/kubexa/kubexa-agent/internal/exec"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// NewExecDialer returns a Dialer that opens ExecSession streams on a gRPC
// connection of its own -- the same address and credentials Connect uses,
// but a separate HTTP/2 connection, so console bytes never share a
// flow-control window with telemetry and a Connect reconnect never
// invalidates an open console. The connection is created on first use and
// kept for the process lifetime; a construction that fails (the CA file
// not yet mounted, say) is retried on the next dial rather than remembered
// until restart. Interceptors are deliberately not attached: they carry
// the Connect stream's metrics and the console has its own log lines.
func NewExecDialer(cfg *config.Config, log *logger.Logger) exec.Dialer {
	if log == nil {
		log = logger.New("exec-dial")
	}
	var (
		mu   sync.Mutex
		conn *grpc.ClientConn
	)
	connect := func() (*grpc.ClientConn, error) {
		mu.Lock()
		defer mu.Unlock()
		if conn != nil {
			return conn, nil
		}
		creds, err := transportCredentials(&cfg.Gateway)
		if err != nil {
			return nil, err
		}
		c, err := grpc.NewClient(cfg.Gateway.Address, grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, err
		}
		log.Info("console connection created", logger.F("address", cfg.Gateway.Address))
		conn = c
		return conn, nil
	}
	return func(ctx context.Context) (agentv1.AgentService_ExecSessionClient, error) {
		c, err := connect()
		if err != nil {
			return nil, err
		}
		return agentv1.NewAgentServiceClient(c).ExecSession(ctx)
	}
}
