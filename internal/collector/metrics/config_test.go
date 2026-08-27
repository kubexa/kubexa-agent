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

func TestConfigFromRootBuildsTheCAdvisorTemplate(t *testing.T) {
	root := &pkgconfig.Config{}
	root.Collect.Metrics.Enabled = true
	root.Collect.Metrics.CAdvisor = pkgconfig.CAdvisorConfig{Enabled: true}
	root.Collect.Metrics.CAdvisor.ApplyDefaults()

	cfg := ConfigFromRoot(root)

	if len(cfg.DynamicTargets.Templates) != 1 {
		t.Fatalf("Templates = %d, want 1", len(cfg.DynamicTargets.Templates))
	}
	tpl := cfg.DynamicTargets.Templates[0]
	if tpl.Kind != "cadvisor" {
		t.Errorf("Kind = %q", tpl.Kind)
	}
	if tpl.Path != "/metrics/cadvisor" || tpl.Scheme != "https" {
		t.Errorf("Scheme/Path = %q %q", tpl.Scheme, tpl.Path)
	}
	if tpl.BearerTokenPath == "" {
		t.Error("BearerTokenPath is empty; the kubelet refuses an unauthenticated scrape with 401")
	}
	// Unfiltered, cAdvisor ships roughly 40 series per container. A default
	// that admits everything makes the first install the cardinality
	// incident this ceiling exists to prevent.
	if len(tpl.MetricAllowlist) == 0 {
		t.Fatal("MetricAllowlist is empty; the default must be a closed list")
	}
	if !containsString(tpl.MetricAllowlist, "^container_cpu_usage_seconds_total$") {
		t.Errorf("MetricAllowlist = %v, missing CPU usage", tpl.MetricAllowlist)
	}
}

func TestCAdvisorDisabledProducesNoTemplate(t *testing.T) {
	root := &pkgconfig.Config{}
	root.Collect.Metrics.Enabled = true
	root.Collect.Metrics.CAdvisor = pkgconfig.CAdvisorConfig{Enabled: false}

	if got := len(ConfigFromRoot(root).DynamicTargets.Templates); got != 0 {
		t.Fatalf("Templates = %d, want 0", got)
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
