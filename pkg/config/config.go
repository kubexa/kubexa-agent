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
	defaultReconnectInitialDelay = 1 * time.Second
	defaultReconnectMaxDelay     = 15 * time.Second
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
	Query         QueryConfig         `yaml:"query"`
	Mutate        MutateConfig        `yaml:"mutate"`
	Exec          ExecConfig          `yaml:"exec"`
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
	Enabled   bool  `yaml:"enabled"`
	TailLines int64 `yaml:"tail_lines"`
	Follow    bool  `yaml:"follow"`
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
	// RedactSecrets controls whether Secret payloads (data/stringData) are stripped from
	// state events before they leave the cluster. Defaults to false: this is the project
	// owner's explicit choice so the Kubexa platform can serve Secret values to cluster
	// admins/owners in the resource explorer. Set to true for an installation that does not
	// want Secret values leaving the cluster at all.
	//
	// This flag never affects metadata scrubbing: managedFields and the
	// kubectl.kubernetes.io/last-applied-configuration annotation are always removed,
	// regardless of RedactSecrets, because that annotation on a kubectl-applied Secret is a
	// second copy of the full base64-encoded payload.
	RedactSecrets bool `yaml:"redact_secrets"`
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
	// CAdvisor scrapes each node's kubelet for container CPU, memory, network
	// and filesystem usage. Its targets are generated from the live node list,
	// not written here: the agent is a single-replica Deployment and a static
	// endpoint list cannot follow nodes joining and leaving.
	CAdvisor CAdvisorConfig `yaml:"cadvisor,omitempty"`
	// KubeStateMetrics scrapes a kube-state-metrics deployment for object
	// state: replica counts, pod phase, restarts, PVC state, HPA and job
	// status. None of it is in the Metrics API.
	KubeStateMetrics KubeStateMetricsConfig `yaml:"kube_state_metrics,omitempty"`
	// MaxSamplesPerScrape caps the samples one scrape of one target may
	// publish. An explicit 0 disables the cap; leaving it unset defaults to
	// DefaultMaxSamplesPerScrape.
	//
	// It is a POINTER because those two are different configurations and an
	// int cannot tell them apart. normalize used to rewrite 0 to 20,000
	// before validation, so the escape hatch that values.yaml, both config
	// comments and applySampleBudget's own contract all promised was
	// unreachable: a tenant with a legitimately wide allowlist set 0, kept
	// the 20,000 cap, and silently went on losing families with
	// dropped_cardinality climbing.
	//
	// This is the agent's own ceiling and it is advisory: the enforcing cap
	// lives in the platform, which is the only side that can bound what a
	// misconfigured or hostile agent sends. It exists so a normal install
	// cannot flood its own uplink, and so the drop is visible to the operator
	// who caused it.
	MaxSamplesPerScrape *int `yaml:"max_samples_per_scrape,omitempty"`
}

// CAdvisorConfig configures per-node kubelet scraping.
type CAdvisorConfig struct {
	Enabled bool `yaml:"enabled"`
	// Interval and Timeout follow the same rule as a custom endpoint: the
	// timeout must stay below the interval.
	Interval time.Duration `yaml:"interval,omitempty"`
	Timeout  time.Duration `yaml:"timeout,omitempty"`
	// RefreshInterval is how often the node inventory is re-listed. It bounds
	// how long a new node waits to be scraped.
	RefreshInterval time.Duration `yaml:"refresh_interval,omitempty"`
	// Port overrides the kubelet port each node reports. Leave empty unless
	// the cluster serves the kubelet somewhere other than where it says.
	Port   string `yaml:"port,omitempty"`
	Scheme string `yaml:"scheme,omitempty"`
	Path   string `yaml:"path,omitempty"`
	// BearerTokenPath and TLS default to the projected ServiceAccount token
	// and CA bundle. The kubelet answers 401 without a token.
	BearerTokenPath string            `yaml:"bearer_token_path,omitempty"`
	TLS             TLSEndpointConfig `yaml:"tls,omitempty"`
	// MetricAllowlist defaults to DefaultCAdvisorAllowlist. Setting it
	// REPLACES that list rather than adding to it -- an operator narrowing the
	// set must be able to narrow it, and a merge would make that impossible.
	MetricAllowlist []string `yaml:"metric_allowlist,omitempty"`
	MetricDenylist  []string `yaml:"metric_denylist,omitempty"`
}

// Default paths for the projected ServiceAccount credentials every pod gets.
const (
	defaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultServiceAccountCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// DefaultCAdvisorAllowlist is the closed set of cAdvisor families this product
// uses. cAdvisor exposes far more, and an open list is what turns a 300-pod
// cluster into roughly 12,000 series.
//
// The patterns are anchored on both ends so `container_memory_usage_bytes`
// cannot be admitted by the working-set entry.
func DefaultCAdvisorAllowlist() []string {
	return []string{
		"^container_cpu_usage_seconds_total$",
		"^container_cpu_cfs_throttled_seconds_total$",
		"^container_memory_working_set_bytes$",
		"^container_memory_rss$",
		"^container_network_receive_bytes_total$",
		"^container_network_transmit_bytes_total$",
		"^container_fs_reads_bytes_total$",
		"^container_fs_writes_bytes_total$",
		"^container_fs_usage_bytes$",
		"^machine_cpu_cores$",
		"^machine_memory_bytes$",
	}
}

// ApplyDefaults fills the zero values a cAdvisor block leaves unset.
func (c *CAdvisorConfig) ApplyDefaults() {
	if c == nil {
		return
	}
	// The CONSTANTS, not the literals validate happens to agree with today.
	// CAdvisorConfig.validate compares effective values against
	// DefaultScrapeInterval/DefaultScrapeTimeout; writing the same numbers out
	// by hand here is the exact drift finding 4 found in the kube-state pair,
	// left structurally possible.
	if c.Interval <= 0 {
		c.Interval = DefaultScrapeInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultScrapeTimeout
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 2 * time.Minute
	}
	if c.Scheme == "" {
		c.Scheme = "https"
	}
	if c.Path == "" {
		c.Path = "/metrics/cadvisor"
	}
	if c.BearerTokenPath == "" {
		c.BearerTokenPath = defaultServiceAccountTokenPath
	}
	if c.TLS.CAFile == "" && !c.TLS.InsecureSkipVerify {
		c.TLS.CAFile = defaultServiceAccountCAPath
	}
	if len(c.MetricAllowlist) == 0 {
		c.MetricAllowlist = DefaultCAdvisorAllowlist()
	}
}

// KubeStateMetricsConfig configures the kube-state-metrics scrape.
type KubeStateMetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	// URL scrapes an explicit address. Leave empty to use ServiceNamespace,
	// ServiceName and Port, which is also what the presence probe checks --
	// an explicit URL disables the probe, because the agent then has no
	// Service to look for.
	URL              string `yaml:"url,omitempty"`
	ServiceNamespace string `yaml:"service_namespace,omitempty"`
	ServiceName      string `yaml:"service_name,omitempty"`
	Port             int32  `yaml:"port,omitempty"`
	Path             string `yaml:"path,omitempty"`

	Interval time.Duration `yaml:"interval,omitempty"`
	Timeout  time.Duration `yaml:"timeout,omitempty"`
	// ProbeInterval is how often absence is re-checked. A cluster that
	// installs kube-state-metrics after the agent must start being scraped
	// without an agent restart.
	ProbeInterval time.Duration `yaml:"probe_interval,omitempty"`

	MetricAllowlist []string `yaml:"metric_allowlist,omitempty"`
	MetricDenylist  []string `yaml:"metric_denylist,omitempty"`
}

// DefaultKubeStateAllowlist is the closed set this product reads.
// kube-state-metrics exposes several hundred families; admitting all of them
// costs more series than cAdvisor does.
func DefaultKubeStateAllowlist() []string {
	return []string{
		"^kube_pod_status_phase$",
		"^kube_pod_container_status_restarts_total$",
		"^kube_pod_container_status_waiting_reason$",
		"^kube_pod_container_resource_requests$",
		"^kube_pod_container_resource_limits$",
		"^kube_deployment_status_replicas$",
		"^kube_deployment_status_replicas_available$",
		"^kube_deployment_spec_replicas$",
		"^kube_statefulset_status_replicas_ready$",
		"^kube_daemonset_status_number_ready$",
		"^kube_daemonset_status_desired_number_scheduled$",
		"^kube_job_status_failed$",
		"^kube_job_status_succeeded$",
		"^kube_persistentvolumeclaim_status_phase$",
		"^kube_horizontalpodautoscaler_status_current_replicas$",
		"^kube_horizontalpodautoscaler_spec_max_replicas$",
		"^kube_node_status_condition$",
		"^kube_node_status_allocatable$",
		"^kube_node_status_capacity$",
	}
}

// ApplyDefaults fills the zero values a kube-state-metrics block leaves unset.
func (k *KubeStateMetricsConfig) ApplyDefaults() {
	if k == nil {
		return
	}
	if k.ServiceNamespace == "" {
		k.ServiceNamespace = "kube-system"
	}
	if k.ServiceName == "" {
		k.ServiceName = "kube-state-metrics"
	}
	if k.Port <= 0 {
		k.Port = 8080
	}
	if k.Path == "" {
		k.Path = "/metrics"
	}
	if k.Interval <= 0 {
		k.Interval = DefaultKubeStateInterval
	}
	if k.Timeout <= 0 {
		k.Timeout = DefaultKubeStateTimeout
	}
	if k.ProbeInterval <= 0 {
		k.ProbeInterval = 5 * time.Minute
	}
	if len(k.MetricAllowlist) == 0 {
		k.MetricAllowlist = DefaultKubeStateAllowlist()
	}
}

// ResolvedURL is the address to scrape.
func (k *KubeStateMetricsConfig) ResolvedURL() string {
	if k == nil {
		return ""
	}
	if k.URL != "" {
		return k.URL
	}
	return fmt.Sprintf("http://%s.%s.svc:%d%s", k.ServiceName, k.ServiceNamespace, k.Port, k.Path)
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

// TLSEndpointConfig configures TLS for one scrape target. It maps onto the
// collector's own TLSConfig; the two are kept separate so the yaml surface can
// change without dragging the collector's internals into pkg/config.
type TLSEndpointConfig struct {
	// InsecureSkipVerify disables certificate verification. The kubelet serves
	// a certificate signed for its node name and IP, which a scrape by IP does
	// not always match; caFile is the correct answer and this is the escape
	// hatch for clusters that cannot produce one.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify,omitempty"`
	// CAFile is a PEM bundle path inside the agent's own filesystem.
	CAFile string `yaml:"ca_file,omitempty"`
}

// MetricEndpointConfig defines a scrape target for custom metrics.
type MetricEndpointConfig struct {
	Name        string            `yaml:"name"`
	URL         string            `yaml:"url"`
	Interval    time.Duration     `yaml:"interval"`
	ExtraLabels map[string]string `yaml:"extra_labels"`
	// Timeout bounds one scrape. It must stay below Interval: a timeout at or
	// above the interval lets a slow target hold its scraper goroutine past the
	// next tick forever, and the target then reports neither success nor
	// failure at its configured rate.
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// BearerTokenPath is read fresh on every scrape, not cached: a projected
	// ServiceAccount token is rotated in place and a cached copy expires.
	BearerTokenPath string            `yaml:"bearer_token_path,omitempty"`
	TLS             TLSEndpointConfig `yaml:"tls,omitempty"`
	// MetricAllowlist and MetricDenylist are RE2 patterns matched against the
	// metric FAMILY name. An empty allowlist admits every family, so leaving
	// both empty on a cAdvisor target ships roughly 40 series per container.
	MetricAllowlist []string `yaml:"metric_allowlist,omitempty"`
	MetricDenylist  []string `yaml:"metric_denylist,omitempty"`
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
	// PprofAddr enables Go profiling on its own listener. Empty means OFF, and
	// that is the default: this agent's heap holds unredacted Secret values from
	// the state watcher's informer cache and raw log lines from every collected
	// pod, so a profile endpoint is an exfiltration path for the data the agent
	// is trusted with. Set it to a loopback address ("127.0.0.1:6060") and it
	// stays unreachable from the cluster network while `kubectl port-forward`,
	// which attaches to the pod's own network namespace, still reaches it.
	PprofAddr string `yaml:"pprof_addr"`
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
				RedactSecrets: false,
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
//
// Keys the agent does not know are dropped without comment. Use
// LoadWithWarnings to hear about them.
func Load(path string) (*Config, error) {
	cfg, _, err := LoadWithWarnings(path)
	return cfg, err
}

// LoadWithWarnings is Load, plus one warning per unrecognized key in the file
// (see UnknownKeys). The warnings are advisory: an unknown key has never
// stopped the agent from starting and still does not, because a stale key in
// a values file an operator has carried forward is not a reason to take their
// telemetry down.
//
// Warnings are returned even when the config is rejected -- an unknown key is
// a plausible reason for a validation failure, so the operator should see both.
func LoadWithWarnings(path string) (*Config, []string, error) {
	cfg := Default()
	var warnings []string

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read config file %q: %w", path, err)
		}
		// Collected before the parse can fail, so a file that carries both a
		// malformed value and an unknown key reports both at once. Otherwise
		// the operator fixes the value, restarts, and only then hears about
		// the key.
		warnings = UnknownKeys(data)
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, warnings, fmt.Errorf("parse config YAML: %w", err)
		}
	}

	applyEnvOverrides(cfg)
	cfg.EnsureAgentID()
	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return nil, warnings, fmt.Errorf("validate config: %w", err)
	}

	return cfg, warnings, nil
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
	violations = append(violations, c.validateQuery()...)
	violations = append(violations, c.validateMutate()...)
	violations = append(violations, c.validateExec()...)

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
