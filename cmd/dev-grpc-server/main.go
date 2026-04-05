// Dev gRPC server: run next to the agent with matching config (e.g. backend.host=127.0.0.1, tls=false).
package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"

	agentv1 "github.com/kubexa/kubexa-agent/gen/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/devbackend"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:50051", "gRPC listen address")
	agentControlDemo := flag.Bool("agent-control-demo", false, "send sample ListNamespaces/ListPods after an agent connects (local test)")
	demoPodNS := flag.String("agent-control-demo-namespace", "stage",
		"preferred namespace for demo ListPods; if it does not exist, first non-kube-* namespace from the cluster is used")
	flag.Parse()

	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Str("component", "dev-grpc-server").Logger()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", *addr).Msg("listen failed")
	}

	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, &devbackend.Server{
		AgentControlDemo:  *agentControlDemo,
		DemoPodNamespace:  *demoPodNS,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info().Str("addr", lis.Addr().String()).Msg("AgentService listening (insecure); stop with Ctrl+C")
		if serveErr := srv.Serve(lis); serveErr != nil {
			log.Error().Err(serveErr).Msg("grpc Serve exited")
			stop()
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutting down dev gRPC server")
	srv.GracefulStop()
}
