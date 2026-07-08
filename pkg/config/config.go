// Package config loads and validates kubexa-agent runtime configuration.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const (
	defaultReconnectInitialDelay = 2 * time.Second
	defaultReconnectMaxDelay     = 60 * time.Second
	defaultDialTimeout           = 10 * time.Second
	defaultHandshakeTimeout      = 10 * time.Second
	defaultTailLines             = int64(100)
	defaultStateResyncPeriod     = 5 * time.Minute
	defaultMaxMemoryBytes        = 64 << 20  // 64 MiB
	defaultMaxDiskBytes          = 512 << 20 // 512 MiB
	defaultBatchSize             = 100
	defaultFlushInterval         = time.Second
	defaultMetricsAddr           = ":9090"
	defaultHealthAddr            = ":8080"
	defaultLogLevel              = "info"
	defaultLogFormat             = "json"
)

var defaultStateResources = []string{"pods", "services", "secrets", "deployments"}

// Config is the root configuration for kubexa-agent.
type Config struct {
	Agent         AgentConfig         `yaml:"agent"`
	Gateway       GatewayConfig       `yaml:"gateway"`
	Collect       CollectConfig       `yaml:"collect"`
	Buffer        BufferConfig        `yaml:"buffer"`
	Observability ObservabilityConfig `yaml:"observability"`
	Log           LogConfig           `yaml:"log"`
}

// AgentConfig identifies the agent instance and tenant.
type AgentConfig struct {
	// TenantToken authenticates the agent with the Kubexa gateway (required).
	TenantToken string `yaml:"tenant_token"`
	// AgentID uniquely identifies this agent process; generated when empty.
	AgentID string `yaml:"agent_id"`
	// ClusterID identifies the Kubernetes cluster; set at runtime from the kube-system namespace UID.
	ClusterID string `yaml:"-"`
}

// GatewayConfig controls connectivity to the Kubexa gateway.
type GatewayConfig struct {
	Address               string        `yaml:"address"`
	TLS                   bool          `yaml:"tls"`
	InsecureSkipVerify    bool          `yaml:"insecure_skip_verify"`
	CACertPath            string        `yaml:"ca_cert_path"`
	ReconnectInitialDelay time.Duration `yaml:"reconnect_initial_delay"`
	ReconnectMaxDelay     time.Duration `yaml:"reconnect_max_delay"`
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	HandshakeTimeout      time.Duration `yaml:"handshake_timeout"`
}

// CollectConfig groups all data collection settings.
type CollectConfig struct {
	Logs    LogsCollectConfig    `yaml:"logs"`
	State   StateCollectConfig   `yaml:"state"`
	Metrics MetricsCollectConfig `yaml:"metrics"`
}

// LogsCollectConfig configures Kubernetes log collection.
type LogsCollectConfig struct {
	Enabled   bool               `yaml:"enabled"`
	TailLines int64              `yaml:"tail_lines"`
	Follow    bool               `yaml:"follow"`
	// CheckpointDir enables SQLite persistence of per-stream read positions.
	// When empty, checkpoints are disabled and tail_lines is used on each new stream.
	CheckpointDir string `yaml:"checkpoint_dir,omitempty"`
	// ExcludeNamespaces skips pods in these namespaces during log discovery.
	ExcludeNamespaces []string           `yaml:"exclude_namespaces,omitempty"`
	Rules             []LogNamespaceRule `yaml:"rules"`
}

// LogNamespaceRule defines log collection settings scoped to a namespace.
type LogNamespaceRule struct {
	// ID uniquely identifies the rule; generated when empty.
	ID string `yaml:"id,omitempty"`
	// Namespace limits collection to this namespace; empty means all namespaces.
	Namespace string `yaml:"namespace"`
	// PodNames limits collection to pods with matching names (supports * suffix wildcards).
	PodNames []string `yaml:"pod_names,omitempty"`
	// LabelSelector filters pods using Kubernetes label selector syntax.
	LabelSelector string `yaml:"label_selector,omitempty"`
	// FieldSelector filters pods using Kubernetes field selector syntax.
	FieldSelector string `yaml:"field_selector,omitempty"`
	// Labels is shorthand for label equality matches; merged into LabelSelector when unset.
	Labels map[string]string `yaml:"labels,omitempty"`
	// Containers limits log streams to named containers; empty collects all containers.
	Containers []string `yaml:"containers,omitempty"`
	// Follow streams logs after the initial tail; nil uses LogsCollectConfig.Follow.
	Follow *bool `yaml:"follow,omitempty"`
	// TailLines overrides the global tail_lines for this rule; nil uses LogsCollectConfig.TailLines.
	TailLines *int64 `yaml:"tail_lines,omitempty"`
}

// StateCollectConfig configures Kubernetes object state collection.
type StateCollectConfig struct {
	Enabled      bool                 `yaml:"enabled"`
	ResyncPeriod time.Duration        `yaml:"resync_period"`
	Rules        []StateNamespaceRule `yaml:"rules"`
}

// StateNamespaceRule defines state collection settings scoped to a namespace.
type StateNamespaceRule struct {
	// ID uniquely identifies the rule; generated when empty.
	ID string `yaml:"id,omitempty"`
	// Namespace limits collection to this namespace; empty means all namespaces.
	Namespace string `yaml:"namespace"`
	// Resources lists Kubernetes resource kinds to watch (e.g. pods, services, ingresses).
	Resources []string `yaml:"resources"`
	// LabelSelector filters watched objects using Kubernetes label selector syntax.
	LabelSelector string `yaml:"label_selector,omitempty"`
	// FieldSelector filters watched objects using Kubernetes field selector syntax.
	FieldSelector string `yaml:"field_selector,omitempty"`
	// ResyncPeriod overrides the global resync_period for this rule; zero uses StateCollectConfig.ResyncPeriod.
	ResyncPeriod time.Duration `yaml:"resync_period,omitempty"`
}

// MetricsCollectConfig configures metrics collection.
type MetricsCollectConfig struct {
	Enabled         bool                   `yaml:"enabled"`
	PodInterval     time.Duration          `yaml:"pod_interval"`
	NodeInterval    time.Duration          `yaml:"node_interval"`
	Rules           []MetricsNamespaceRule `yaml:"rules"`
	CustomEndpoints []MetricEndpointConfig `yaml:"custom_endpoints"`
	// KubeMetrics is deprecated; use rules instead. When true and rules is empty,
	// normalize creates a cluster-wide pods+nodes rule for backward compatibility.
	KubeMetrics bool `yaml:"kube_metrics,omitempty"`
}

// MetricsNamespaceRule defines Kubernetes Metrics API collection scoped by namespace and filters.
type MetricsNamespaceRule struct {
	// ID uniquely identifies the rule; generated when empty.
	ID string `yaml:"id,omitempty"`
	// Namespace limits pod metrics to this namespace; empty means all namespaces.
	Namespace string `yaml:"namespace"`
	// Resources lists metrics resources to scrape (pods, nodes).
	Resources []string `yaml:"resources"`
	// PodNames limits pod metrics to matching pod names (supports * suffix wildcards).
	PodNames []string `yaml:"pod_names,omitempty"`
	// NodeNames limits node metrics to matching node names (supports * suffix wildcards).
	NodeNames []string `yaml:"node_names,omitempty"`
	// LabelSelector filters pods using Kubernetes label selector syntax.
	LabelSelector string `yaml:"label_selector,omitempty"`
	// FieldSelector filters pods using Kubernetes field selector syntax.
	FieldSelector string `yaml:"field_selector,omitempty"`
	// Labels is shorthand for label equality matches; merged into LabelSelector when unset.
	Labels map[string]string `yaml:"labels,omitempty"`
	// PodInterval overrides MetricsCollectConfig.PodInterval for this rule.
	PodInterval time.Duration `yaml:"pod_interval,omitempty"`
	// NodeInterval overrides MetricsCollectConfig.NodeInterval for this rule.
	NodeInterval time.Duration `yaml:"node_interval,omitempty"`
}

// MetricEndpointConfig defines a scrape target for custom metrics.
type MetricEndpointConfig struct {
	Name        string            `yaml:"name"`
	URL         string            `yaml:"url"`
	Interval    time.Duration     `yaml:"interval"`
	ExtraLabels map[string]string `yaml:"extra_labels"`
}

// BufferConfig controls in-memory and on-disk buffering before export.
type BufferConfig struct {
	MaxMemoryBytes int64         `yaml:"max_memory_bytes"`
	SpillDir       string        `yaml:"spill_dir"`
	MaxDiskBytes   int64         `yaml:"max_disk_bytes"`
	BatchSize      int           `yaml:"batch_size"`
	FlushInterval  time.Duration `yaml:"flush_interval"`
}

// ObservabilityConfig exposes agent self-metrics and health endpoints.
type ObservabilityConfig struct {
	MetricsAddr string `yaml:"metrics_addr"`
	HealthAddr  string `yaml:"health_addr"`
}

// LogConfig configures the agent process logger.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// NamespaceUIDGetter resolves a Kubernetes namespace UID by name.
type NamespaceUIDGetter interface {
	NamespaceUID(ctx context.Context, name string) (string, error)
}

// Default returns a fully populated configuration with documented defaults.
func Default() *Config {
	resources := make([]string, len(defaultStateResources))
	copy(resources, defaultStateResources)

	return &Config{
		Agent: AgentConfig{},
		Gateway: GatewayConfig{
			TLS:                   true,
			InsecureSkipVerify:    false,
			ReconnectInitialDelay: defaultReconnectInitialDelay,
			ReconnectMaxDelay:     defaultReconnectMaxDelay,
			DialTimeout:           defaultDialTimeout,
			HandshakeTimeout:      defaultHandshakeTimeout,
		},
		Collect: CollectConfig{
			Logs: LogsCollectConfig{
				Enabled:   true,
				TailLines: defaultTailLines,
				Follow:    true,
				Rules: []LogNamespaceRule{
					{Namespace: ""},
				},
			},
			State: StateCollectConfig{
				Enabled:      true,
				ResyncPeriod: defaultStateResyncPeriod,
				Rules: []StateNamespaceRule{
					{Resources: resources},
				},
			},
			Metrics: MetricsCollectConfig{
				Enabled: true,
			},
		},
		Buffer: BufferConfig{
			MaxMemoryBytes: defaultMaxMemoryBytes,
			MaxDiskBytes:   defaultMaxDiskBytes,
			BatchSize:      defaultBatchSize,
			FlushInterval:  defaultFlushInterval,
		},
		Observability: ObservabilityConfig{
			MetricsAddr: defaultMetricsAddr,
			HealthAddr:  defaultHealthAddr,
		},
		Log: LogConfig{
			Level:  defaultLogLevel,
			Format: defaultLogFormat,
		},
	}
}

// Load reads configuration from path (when non-empty), applies environment overrides,
// ensures agent_id is set, and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config file %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config YAML: %w", err)
		}
	}

	applyEnvOverrides(cfg)
	cfg.EnsureAgentID()
	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// LoadFromYAML parses configuration from YAML bytes (used in tests and tooling).
func LoadFromYAML(data []byte) (*Config, error) {
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config YAML: %w", err)
	}
	applyEnvOverrides(cfg)
	cfg.EnsureAgentID()
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

// EnsureAgentID assigns a new UUID to agent.agent_id when it is empty.
func (c *Config) EnsureAgentID() {
	if c == nil || c.Agent.AgentID != "" {
		return
	}
	c.Agent.AgentID = uuid.NewString()
}

// EnsureClusterID sets agent.cluster_id from the kube-system namespace UID.
// The value is always derived from the cluster at runtime and cannot be configured at install time.
func (c *Config) EnsureClusterID(ctx context.Context, getter NamespaceUIDGetter) error {
	if c == nil {
		return errors.New("config is nil")
	}
	if getter == nil {
		return errors.New("namespace UID getter is nil")
	}

	uid, err := getter.NamespaceUID(ctx, "kube-system")
	if err != nil {
		return fmt.Errorf("resolve cluster_id from kube-system: %w", err)
	}
	if uid == "" {
		return errors.New("kube-system namespace UID is empty")
	}

	c.Agent.ClusterID = uid
	return nil
}

// Validate checks required fields and collection settings.
func (c *Config) Validate() error {
	if c == nil {
		return &ValidationError{Violations: []string{"config is nil"}}
	}

	c.Normalize()

	var violations []string

	if strings.TrimSpace(c.Agent.TenantToken) == "" {
		violations = append(violations, "agent.tenant_token must not be empty")
	}
	if strings.TrimSpace(c.Gateway.Address) == "" {
		violations = append(violations, "gateway.address must not be empty")
	}
	if c.Gateway.TLS && c.Gateway.InsecureSkipVerify {
		violations = append(violations, "gateway.insecure_skip_verify is only allowed when gateway.tls is false")
	}
	if c.Buffer.BatchSize <= 0 {
		violations = append(violations, "buffer.batch_size must be greater than 0")
	}
	if c.Buffer.MaxMemoryBytes <= 0 {
		violations = append(violations, "buffer.max_memory_bytes must be greater than 0")
	}
	if !c.Collect.Logs.Enabled && !c.Collect.State.Enabled && !c.Collect.Metrics.Enabled {
		violations = append(violations, "at least one of collect.logs, collect.state, or collect.metrics must be enabled")
	}

	violations = append(violations, c.Collect.Logs.validate()...)
	violations = append(violations, c.Collect.State.validate()...)
	violations = append(violations, c.Collect.Metrics.validate()...)

	if len(violations) == 0 {
		return nil
	}
	return &ValidationError{Violations: violations}
}

// ValidationError aggregates configuration validation failures.
type ValidationError struct {
	Violations []string
}

// Error returns all validation violations in a single message.
func (e *ValidationError) Error() string {
	if e == nil || len(e.Violations) == 0 {
		return "invalid configuration"
	}
	return "invalid configuration: " + strings.Join(e.Violations, "; ")
}

// Redacted returns a copy of the configuration with sensitive fields masked.
func (c *Config) Redacted() *Config {
	if c == nil {
		return nil
	}
	out := *c
	if out.Agent.TenantToken != "" {
		out.Agent.TenantToken = "***"
	}
	return &out
}

// String returns a YAML representation with sensitive fields redacted.
func (c *Config) String() string {
	data, err := yaml.Marshal(c.Redacted())
	if err != nil {
		return fmt.Sprintf("config: marshal error: %v", err)
	}
	return string(data)
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("KUBEXA_TENANT_TOKEN"); v != "" {
		cfg.Agent.TenantToken = v
	}
	if v := os.Getenv("KUBEXA_AGENT_ID"); v != "" {
		cfg.Agent.AgentID = v
	}
	if v := os.Getenv("KUBEXA_GATEWAY_ADDRESS"); v != "" {
		cfg.Gateway.Address = v
	}
	if v := os.Getenv("KUBEXA_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
}
