package logs

import (
	"time"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

const (
	defaultResyncPeriod         = 60 * time.Second
	defaultMaxConcurrentStreams = 200
	defaultTailLines            = int64(100)
	defaultWriteTimeout         = 100 * time.Millisecond
)

// Config holds runtime settings for the log collector.
// Collection rules are sourced from pkg/config.LogsCollectConfig.
type Config struct {
	pkgconfig.LogsCollectConfig

	// CheckpointDir persists per-stream log read positions (empty disables checkpoints).
	CheckpointDir string
	// ResyncPeriod controls how often pods are re-listed to discover new targets.
	ResyncPeriod time.Duration
	// MaxConcurrentStreams limits simultaneous pod log streams.
	MaxConcurrentStreams int64
	// WriteTimeout bounds non-blocking queue writes.
	WriteTimeout time.Duration
}

// DefaultConfig returns collector defaults layered on pkg/config log defaults.
func DefaultConfig() Config {
	return Config{
		LogsCollectConfig: pkgconfig.LogsCollectConfig{
			Enabled:   true,
			TailLines: defaultTailLines,
			Follow:    true,
		},
		ResyncPeriod:         defaultResyncPeriod,
		MaxConcurrentStreams: defaultMaxConcurrentStreams,
		WriteTimeout:         defaultWriteTimeout,
	}
}

// ConfigFromRoot maps the agent root configuration into collector settings.
func ConfigFromRoot(root *pkgconfig.Config) Config {
	if root == nil {
		return DefaultConfig()
	}
	cfg := Config{
		LogsCollectConfig:    root.Collect.Logs,
		CheckpointDir:        root.Collect.Logs.CheckpointDir,
		ResyncPeriod:         defaultResyncPeriod,
		MaxConcurrentStreams: defaultMaxConcurrentStreams,
		WriteTimeout:         defaultWriteTimeout,
	}
	cfg.ApplyDefaults()
	return cfg
}

// ApplyDefaults fills zero values with documented defaults.
func (c *Config) ApplyDefaults() {
	if c == nil {
		return
	}
	if c.TailLines <= 0 {
		c.TailLines = defaultTailLines
	}
	if c.ResyncPeriod <= 0 {
		c.ResyncPeriod = defaultResyncPeriod
	}
	if c.MaxConcurrentStreams <= 0 {
		c.MaxConcurrentStreams = defaultMaxConcurrentStreams
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaultWriteTimeout
	}
}

// Enabled reports whether log collection is active.
func (c *Config) Enabled() bool {
	return c != nil && c.LogsCollectConfig.Enabled
}
