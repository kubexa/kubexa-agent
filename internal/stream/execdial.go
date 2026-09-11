package stream

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

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
// kept for the process lifetime. Interceptors are deliberately not
// attached: they carry the Connect stream's metrics and the console has its
// own log lines.
func NewExecDialer(cfg *config.Config, log *logger.Logger) exec.Dialer {
	if log == nil {
		log = logger.New("exec-dial")
	}
	var (
		once sync.Once
		conn *grpc.ClientConn
		err  error
	)
	return func(ctx context.Context) (agentv1.AgentService_ExecSessionClient, error) {
		once.Do(func() {
			var creds credentials.TransportCredentials
			creds, err = transportCredentials(&cfg.Gateway)
			if err != nil {
				return
			}
			conn, err = grpc.NewClient(cfg.Gateway.Address, grpc.WithTransportCredentials(creds))
			if err == nil {
				log.Info("console connection created", logger.F("address", cfg.Gateway.Address))
			}
		})
		if err != nil {
			return nil, err
		}
		return agentv1.NewAgentServiceClient(conn).ExecSession(ctx)
	}
}
