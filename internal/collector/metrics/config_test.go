package metrics

import (
	"testing"
	"time"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

func TestConfigFromRootCarriesEveryCustomEndpointField(t *testing.T) {
	root := &pkgconfig.Config{}
	root.Collect.Metrics.Enabled = true
	root.Collect.Metrics.CustomEndpoints = []pkgconfig.MetricEndpointConfig{{
		Name:            "kubelet",
		URL:             "https://10.0.0.1:10250/metrics/cadvisor",
		Interval:        45 * time.Second,
		Timeout:         12 * time.Second,
		ExtraLabels:     map[string]string{"source": "kubelet"},
		BearerTokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		TLS: pkgconfig.TLSEndpointConfig{
			InsecureSkipVerify: true,
			CAFile:             "/etc/ssl/ca.crt",
		},
		MetricAllowlist: []string{"^container_cpu_.*"},
		MetricDenylist:  []string{"^container_tasks_.*"},
	}}

	cfg := ConfigFromRoot(root)

	if len(cfg.CustomTargets) != 1 {
		t.Fatalf("CustomTargets = %d, want 1", len(cfg.CustomTargets))
	}
	got := cfg.CustomTargets[0]
	if got.Timeout != 12*time.Second {
		t.Errorf("Timeout = %s, want 12s", got.Timeout)
	}
	if got.BearerTokenPath != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Errorf("BearerTokenPath = %q", got.BearerTokenPath)
	}
	if !got.TLSConfig.InsecureSkipVerify || got.TLSConfig.CAFile != "/etc/ssl/ca.crt" {
		t.Errorf("TLSConfig = %+v", got.TLSConfig)
	}
	if len(got.MetricAllowlist) != 1 || got.MetricAllowlist[0] != "^container_cpu_.*" {
		t.Errorf("MetricAllowlist = %v", got.MetricAllowlist)
	}
	if len(got.MetricDenylist) != 1 || got.MetricDenylist[0] != "^container_tasks_.*" {
		t.Errorf("MetricDenylist = %v", got.MetricDenylist)
	}
}
