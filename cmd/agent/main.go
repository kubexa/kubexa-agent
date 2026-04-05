package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/kubexa/kubexa-agent/internal/agent"
	"github.com/kubexa/kubexa-agent/internal/config"
)

// version is set by -ldflags at link time (e.g. Dockerfile).
var version = "dev"

var configPath string

func main() {
	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "kubexa-agent",
		Short:   "Stream Kubernetes pod events and logs to the Kubexa backend",
		Version: version,
		Args:    cobra.NoArgs,
		Run:     runAgent,
	}
	cmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to YAML config file (default: /etc/kubexa/config.yaml or ./config/config.yaml)")
	return cmd
}

func runAgent(_ *cobra.Command, _ []string) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()

	log.Info().Str("version", version).Msg("starting kubexa-agent")

	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("config load failed")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ag := agent.NewAgent(cfg)
	if err := ag.RunWait(ctx); err != nil {
		log.Fatal().Err(err).Msg("agent exited with error")
	}

	log.Info().Msg("kubexa-agent stopped")
}
