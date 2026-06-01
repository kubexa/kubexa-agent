package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func TestDefault(t *testing.T) {
	cfg := config.Default()

	if cfg.Gateway.TLS != true {
		t.Errorf("gateway.tls = %v, want true", cfg.Gateway.TLS)
	}
	if cfg.Gateway.InsecureSkipVerify != false {
		t.Errorf("gateway.insecure_skip_verify = %v, want false", cfg.Gateway.InsecureSkipVerify)
	}
	if cfg.Gateway.ReconnectInitialDelay != 2*time.Second {
		t.Errorf("gateway.reconnect_initial_delay = %v, want 2s", cfg.Gateway.ReconnectInitialDelay)
	}
	if cfg.Gateway.ReconnectMaxDelay != 60*time.Second {
		t.Errorf("gateway.reconnect_max_delay = %v, want 60s", cfg.Gateway.ReconnectMaxDelay)
	}
	if cfg.Gateway.DialTimeout != 10*time.Second {
		t.Errorf("gateway.dial_timeout = %v, want 10s", cfg.Gateway.DialTimeout)
	}

	if !cfg.Collect.Logs.Enabled {
		t.Error("collect.logs.enabled = false, want true")
	}
	if cfg.Collect.Logs.TailLines != 100 {
		t.Errorf("collect.logs.tail_lines = %d, want 100", cfg.Collect.Logs.TailLines)
	}

	if !cfg.Collect.State.Enabled {
		t.Error("collect.state.enabled = false, want true")
	}
	if cfg.Collect.State.ResyncPeriod != 5*time.Minute {
		t.Errorf("collect.state.resync_period = %v, want 5m", cfg.Collect.State.ResyncPeriod)
	}
	wantResources := []string{"pods", "services", "secrets", "deployments"}
	if len(cfg.Collect.State.Resources) != len(wantResources) {
		t.Fatalf("collect.state.resources len = %d, want %d", len(cfg.Collect.State.Resources), len(wantResources))
	}
	for i, r := range wantResources {
		if cfg.Collect.State.Resources[i] != r {
			t.Errorf("collect.state.resources[%d] = %q, want %q", i, cfg.Collect.State.Resources[i], r)
		}
	}

	if !cfg.Collect.Metrics.Enabled || !cfg.Collect.Metrics.KubeMetrics {
		t.Errorf("collect.metrics = %+v, want enabled and kube_metrics true", cfg.Collect.Metrics)
	}

	if cfg.Buffer.MaxMemoryBytes != 64<<20 {
		t.Errorf("buffer.max_memory_bytes = %d, want %d", cfg.Buffer.MaxMemoryBytes, 64<<20)
	}
	if cfg.Buffer.MaxDiskBytes != 512<<20 {
		t.Errorf("buffer.max_disk_bytes = %d, want %d", cfg.Buffer.MaxDiskBytes, 512<<20)
	}
	if cfg.Buffer.BatchSize != 100 {
		t.Errorf("buffer.batch_size = %d, want 100", cfg.Buffer.BatchSize)
	}
	if cfg.Buffer.FlushInterval != time.Second {
		t.Errorf("buffer.flush_interval = %v, want 1s", cfg.Buffer.FlushInterval)
	}

	if cfg.Observability.MetricsAddr != ":9090" || cfg.Observability.HealthAddr != ":8080" {
		t.Errorf("observability = %+v, want :9090 and :8080", cfg.Observability)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("log = %+v, want level info and format json", cfg.Log)
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	const yamlBody = `
agent:
  tenant_token: yaml-token
  agent_id: yaml-agent
  cluster_id: yaml-cluster
gateway:
  address: gateway.example.com:443
  tls: false
  reconnect_initial_delay: 3s
  dial_timeout: 15s
collect:
  logs:
    enabled: true
    tail_lines: 50
  state:
    enabled: false
  metrics:
    enabled: true
    kube_metrics: false
    custom_endpoints:
      - name: app
        url: http://localhost:8080/metrics
        interval: 30s
        extra_labels:
          env: test
buffer:
  batch_size: 200
  flush_interval: 2s
observability:
  metrics_addr: ":9100"
log:
  level: debug
  format: console
`
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Agent.TenantToken != "yaml-token" {
		t.Errorf("tenant_token = %q, want yaml-token", cfg.Agent.TenantToken)
	}
	if cfg.Agent.AgentID != "yaml-agent" {
		t.Errorf("agent_id = %q, want yaml-agent", cfg.Agent.AgentID)
	}
	if cfg.Gateway.Address != "gateway.example.com:443" {
		t.Errorf("gateway.address = %q", cfg.Gateway.Address)
	}
	if cfg.Gateway.TLS {
		t.Error("gateway.tls = true, want false from YAML")
	}
	if cfg.Gateway.ReconnectInitialDelay != 3*time.Second {
		t.Errorf("reconnect_initial_delay = %v, want 3s", cfg.Gateway.ReconnectInitialDelay)
	}
	if cfg.Gateway.DialTimeout != 15*time.Second {
		t.Errorf("dial_timeout = %v, want 15s", cfg.Gateway.DialTimeout)
	}
	if cfg.Collect.Logs.TailLines != 50 {
		t.Errorf("tail_lines = %d, want 50", cfg.Collect.Logs.TailLines)
	}
	if cfg.Collect.State.Enabled {
		t.Error("collect.state.enabled = true, want false")
	}
	if cfg.Collect.Metrics.KubeMetrics {
		t.Error("collect.metrics.kube_metrics = true, want false")
	}
	if len(cfg.Collect.Metrics.CustomEndpoints) != 1 {
		t.Fatalf("custom_endpoints len = %d, want 1", len(cfg.Collect.Metrics.CustomEndpoints))
	}
	ep := cfg.Collect.Metrics.CustomEndpoints[0]
	if ep.Name != "app" || ep.URL != "http://localhost:8080/metrics" || ep.Interval != 30*time.Second {
		t.Errorf("custom endpoint = %+v", ep)
	}
	if ep.ExtraLabels["env"] != "test" {
		t.Errorf("extra_labels = %v", ep.ExtraLabels)
	}
	if cfg.Buffer.BatchSize != 200 || cfg.Buffer.FlushInterval != 2*time.Second {
		t.Errorf("buffer = %+v", cfg.Buffer)
	}
	if cfg.Observability.MetricsAddr != ":9100" {
		t.Errorf("metrics_addr = %q", cfg.Observability.MetricsAddr)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "console" {
		t.Errorf("log = %+v", cfg.Log)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("KUBEXA_TENANT_TOKEN", "env-token")
	t.Setenv("KUBEXA_AGENT_ID", "env-agent")
	t.Setenv("KUBEXA_CLUSTER_ID", "env-cluster")
	t.Setenv("KUBEXA_GATEWAY_ADDRESS", "env.gateway:443")
	t.Setenv("KUBEXA_LOG_LEVEL", "warn")

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	const yamlBody = `
agent:
  tenant_token: yaml-token
  agent_id: yaml-agent
  cluster_id: yaml-cluster
gateway:
  address: yaml.gateway:443
log:
  level: info
`
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Agent.TenantToken != "env-token" {
		t.Errorf("tenant_token = %q, want env-token", cfg.Agent.TenantToken)
	}
	if cfg.Agent.AgentID != "env-agent" {
		t.Errorf("agent_id = %q, want env-agent", cfg.Agent.AgentID)
	}
	if cfg.Agent.ClusterID != "env-cluster" {
		t.Errorf("cluster_id = %q, want env-cluster", cfg.Agent.ClusterID)
	}
	if cfg.Gateway.Address != "env.gateway:443" {
		t.Errorf("gateway.address = %q, want env.gateway:443", cfg.Gateway.Address)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", cfg.Log.Level)
	}
}

func TestLoadGeneratesAgentID(t *testing.T) {
	t.Setenv("KUBEXA_TENANT_TOKEN", "token")
	t.Setenv("KUBEXA_GATEWAY_ADDRESS", "gw:443")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Agent.AgentID == "" {
		t.Fatal("agent_id is empty, expected auto-generated UUID")
	}
	if _, err := uuid.Parse(cfg.Agent.AgentID); err != nil {
		t.Errorf("agent_id %q is not a valid UUID: %v", cfg.Agent.AgentID, err)
	}
}

func TestValidateCombinedErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.TenantToken = ""
	cfg.Gateway.Address = ""
	cfg.Buffer.BatchSize = 0
	cfg.Buffer.MaxMemoryBytes = 0
	cfg.Collect.Logs.Enabled = false
	cfg.Collect.State.Enabled = false
	cfg.Collect.Metrics.Enabled = false

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}

	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As ValidationError = false for %T", err)
	}

	wantSubs := []string{
		"agent.tenant_token",
		"gateway.address",
		"buffer.batch_size",
		"buffer.max_memory_bytes",
		"at least one of collect.logs",
	}
	msg := err.Error()
	for _, sub := range wantSubs {
		if !strings.Contains(msg, sub) {
			t.Errorf("error %q missing substring %q", msg, sub)
		}
	}
	if len(ve.Violations) < len(wantSubs) {
		t.Errorf("violations count = %d, want at least %d: %v", len(ve.Violations), len(wantSubs), ve.Violations)
	}
}

func TestRedacted(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.TenantToken = "super-secret"
	cfg.Gateway.Address = "gw:443"

	redacted := cfg.Redacted()
	if redacted.Agent.TenantToken != "***" {
		t.Errorf("redacted tenant_token = %q, want ***", redacted.Agent.TenantToken)
	}
	if cfg.Agent.TenantToken != "super-secret" {
		t.Error("original tenant_token was mutated")
	}

	s := cfg.String()
	if strings.Contains(s, "super-secret") {
		t.Errorf("String() leaked secret: %s", s)
	}
	if !strings.Contains(s, "***") {
		t.Errorf("String() = %q, want redacted token", s)
	}
}

func TestEnsureClusterID(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.ClusterID = ""

	getter := &stubNamespaceGetter{uid: "cluster-uid-123"}
	if err := cfg.EnsureClusterID(context.Background(), getter); err != nil {
		t.Fatalf("EnsureClusterID() error = %v", err)
	}
	if cfg.Agent.ClusterID != "cluster-uid-123" {
		t.Errorf("cluster_id = %q, want cluster-uid-123", cfg.Agent.ClusterID)
	}
	if getter.calls != 1 || getter.lastName != "kube-system" {
		t.Errorf("getter calls = %d name = %q", getter.calls, getter.lastName)
	}

	// No-op when already set.
	cfg.Agent.ClusterID = "existing"
	if err := cfg.EnsureClusterID(context.Background(), getter); err != nil {
		t.Fatalf("EnsureClusterID() error = %v", err)
	}
	if cfg.Agent.ClusterID != "existing" {
		t.Error("cluster_id was overwritten")
	}
}

type stubNamespaceGetter struct {
	uid      string
	err      error
	calls    int
	lastName string
}

func (s *stubNamespaceGetter) NamespaceUID(_ context.Context, name string) (string, error) {
	s.calls++
	s.lastName = name
	if s.err != nil {
		return "", s.err
	}
	return s.uid, nil
}
