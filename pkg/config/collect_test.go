package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func TestLogsCollectConfig_validateCheckpointDir(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Collect.Logs.Enabled = true
	cfg.Collect.Logs.CheckpointDir = "/var/../etc"
	cfg.Normalize()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	if !strings.Contains(err.Error(), "collect.logs.checkpoint_dir must not contain ..") {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestLogNamespaceRuleEffectiveLabelSelector(t *testing.T) {
	rule := config.LogNamespaceRule{
		Labels: map[string]string{"app": "api", "tier": "backend"},
	}
	got := rule.EffectiveLabelSelector()
	want := "app=api,tier=backend"
	if got != want {
		t.Errorf("EffectiveLabelSelector() = %q, want %q", got, want)
	}

	rule = config.LogNamespaceRule{
		LabelSelector: "app=web",
		Labels:        map[string]string{"ignored": "true"},
	}
	if got := rule.EffectiveLabelSelector(); got != "app=web" {
		t.Errorf("LabelSelector override = %q, want app=web", got)
	}
}

func TestParseResourceKind(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    agentv1.ResourceKind
		wantErr bool
	}{
		{"pods", "pods", agentv1.ResourceKind_RESOURCE_KIND_POD, false},
		{"ingress", "ingress", agentv1.ResourceKind_RESOURCE_KIND_INGRESS, false},
		{"ingresses", "ingresses", agentv1.ResourceKind_RESOURCE_KIND_INGRESS, false},
		{"jobs", "jobs", agentv1.ResourceKind_RESOURCE_KIND_JOB, false},
		{"cronjobs", "cronjobs", agentv1.ResourceKind_RESOURCE_KIND_CRONJOB, false},
		{"unknown", "not-a-resource", agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.ParseResourceKind(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseResourceKind() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseResourceKind() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadNamespaceRulesYAML(t *testing.T) {
	const yamlBody = `
agent:
  tenant_token: token
gateway:
  address: gw:443
collect:
  logs:
    enabled: true
    tail_lines: 200
    follow: true
    exclude_namespaces:
      - kube-system
      - kube-system
    rules:
      - id: prod-api
        namespace: production
        pod_names:
          - api-server-*
        label_selector: app=api
        containers:
          - api
        tail_lines: 50
      - namespace: staging
        labels:
          tier: backend
  state:
    enabled: true
    resync_period: 10m
    rules:
      - id: prod-core
        namespace: production
        resources:
          - pods
          - services
      - namespace: ingress-nginx
        resources:
          - ingresses
        resync_period: 2m
`
	cfg, err := config.LoadFromYAML([]byte(yamlBody))
	if err != nil {
		t.Fatalf("LoadFromYAML() error = %v", err)
	}

	if len(cfg.Collect.Logs.ExcludeNamespaces) != 1 || cfg.Collect.Logs.ExcludeNamespaces[0] != "kube-system" {
		t.Errorf("exclude_namespaces = %v, want [kube-system]", cfg.Collect.Logs.ExcludeNamespaces)
	}
	if len(cfg.Collect.Logs.Rules) != 2 {
		t.Fatalf("log rules len = %d, want 2", len(cfg.Collect.Logs.Rules))
	}
	logProd := cfg.Collect.Logs.Rules[0]
	if logProd.ID != "prod-api" || logProd.Namespace != "production" {
		t.Errorf("log rule[0] = %+v", logProd)
	}
	if len(logProd.PodNames) != 1 || logProd.PodNames[0] != "api-server-*" {
		t.Errorf("pod_names = %v", logProd.PodNames)
	}
	if logProd.ResolveTailLines(cfg.Collect.Logs.TailLines) != 50 {
		t.Errorf("tail_lines = %d, want 50", logProd.ResolveTailLines(cfg.Collect.Logs.TailLines))
	}
	logStaging := cfg.Collect.Logs.Rules[1]
	if logStaging.EffectiveLabelSelector() != "tier=backend" {
		t.Errorf("staging label selector = %q", logStaging.EffectiveLabelSelector())
	}

	if len(cfg.Collect.State.Rules) != 2 {
		t.Fatalf("state rules len = %d, want 2", len(cfg.Collect.State.Rules))
	}
	stateProd := cfg.Collect.State.Rules[0]
	if stateProd.ID != "prod-core" || len(stateProd.Resources) != 2 {
		t.Errorf("state rule[0] = %+v", stateProd)
	}
	stateIngress := cfg.Collect.State.Rules[1]
	if stateIngress.Namespace != "ingress-nginx" {
		t.Errorf("ingress namespace = %q", stateIngress.Namespace)
	}
	if stateIngress.ResolveResyncPeriod(cfg.Collect.State.ResyncPeriod) != 2*time.Minute {
		t.Errorf("resync = %v, want 2m", stateIngress.ResolveResyncPeriod(cfg.Collect.State.ResyncPeriod))
	}

	snap, err := cfg.ToProtoSnapshot()
	if err != nil {
		t.Fatalf("ToProtoSnapshot() error = %v", err)
	}
	if len(snap.LogCollectors) != 2 {
		t.Fatalf("proto log collectors = %d, want 2", len(snap.LogCollectors))
	}
	if snap.LogCollectors[0].PodSelectors[0] != "app=api" {
		t.Errorf("proto pod selector = %v", snap.LogCollectors[0].PodSelectors)
	}
	if len(snap.Watchers) != 2 {
		t.Fatalf("proto watchers = %d, want 2", len(snap.Watchers))
	}
	if snap.Watchers[1].Kinds[0] != agentv1.ResourceKind_RESOURCE_KIND_INGRESS {
		t.Errorf("ingress kind = %v", snap.Watchers[1].Kinds[0])
	}
}

func TestValidateStateRuleResources(t *testing.T) {
	cfg := config.Default()
	cfg.Collect.State.Rules = []config.StateNamespaceRule{
		{ID: "bad", Namespace: "default", Resources: []string{"invalid-kind"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for invalid resource")
	}
}

func TestMetricsNamespaceRuleIncludesResources(t *testing.T) {
	rule := config.MetricsNamespaceRule{Resources: []string{"pods", "nodes"}}
	if !rule.IncludesPods() || !rule.IncludesNodes() {
		t.Fatalf("IncludesPods/IncludesNodes = false, want true for %+v", rule)
	}
}

func TestValidateMetricsRuleResources(t *testing.T) {
	cfg := config.Default()
	cfg.Collect.Metrics.Rules = []config.MetricsNamespaceRule{
		{ID: "bad", Namespace: "default", Resources: []string{"deployments"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for invalid metrics resource")
	}
}

func TestMetricsKubeMetricsLegacyNormalize(t *testing.T) {
	cfg := config.Default()
	cfg.Collect.Metrics.Rules = nil
	cfg.Collect.Metrics.KubeMetrics = true
	cfg.Normalize()
	if len(cfg.Collect.Metrics.Rules) != 1 {
		t.Fatalf("rules len = %d, want 1 after kube_metrics normalize", len(cfg.Collect.Metrics.Rules))
	}
	if !cfg.Collect.Metrics.Rules[0].IncludesPods() || !cfg.Collect.Metrics.Rules[0].IncludesNodes() {
		t.Errorf("legacy rule = %+v", cfg.Collect.Metrics.Rules[0])
	}
}

func TestMetricsCustomEndpointValidation(t *testing.T) {
	cases := []struct {
		name      string
		endpoint  config.MetricEndpointConfig
		want      string
		wantValid bool
	}{
		{
			name:     "missing url",
			endpoint: config.MetricEndpointConfig{Name: "a"},
			want:     "url must be set",
		},
		{
			name:     "non-http scheme",
			endpoint: config.MetricEndpointConfig{Name: "a", URL: "tcp://host:9100/metrics"},
			want:     "url must use http or https",
		},
		{
			name: "timeout not below interval",
			endpoint: config.MetricEndpointConfig{
				Name: "a", URL: "http://h:9100/metrics",
				Interval: 30 * time.Second, Timeout: 30 * time.Second,
			},
			want: "timeout must be below interval",
		},
		{
			name: "bad allowlist pattern",
			endpoint: config.MetricEndpointConfig{
				Name: "a", URL: "http://h:9100/metrics",
				MetricAllowlist: []string{"("},
			},
			want: "metric_allowlist[0] is not a valid regular expression",
		},
		{
			// Interval is written, Timeout is not: ApplyDefaults fills the
			// omitted Timeout with DefaultScrapeTimeout (10s), which is not
			// below the 5s interval the operator actually wrote. Validation
			// has to catch this from the EFFECTIVE values, not just the ones
			// present in the yaml.
			name: "defaulted timeout not below a short interval",
			endpoint: config.MetricEndpointConfig{
				Name: "a", URL: "http://h:9100/metrics",
				Interval: 5 * time.Second,
			},
			want: "timeout must be below interval",
		},
		{
			// Neither field is written: the defaults are 30s/10s, which
			// satisfy the invariant on their own. The defaulting added by
			// the fix above must not start refusing the default config.
			name: "unset interval and timeout use valid defaults",
			endpoint: config.MetricEndpointConfig{
				Name: "a", URL: "http://h:9100/metrics",
			},
			wantValid: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Collect.Metrics.Enabled = true
			cfg.Collect.Metrics.CustomEndpoints = []config.MetricEndpointConfig{tc.endpoint}

			violations := config.ValidateForTest(cfg)
			if tc.wantValid {
				if len(violations) != 0 {
					t.Fatalf("violations = %v, want none", violations)
				}
				return
			}
			if !containsSubstring(violations, tc.want) {
				t.Fatalf("violations = %v, want one containing %q", violations, tc.want)
			}
		})
	}
}

func TestCAdvisorValidation(t *testing.T) {
	cases := []struct {
		name      string
		cadvisor  config.CAdvisorConfig
		want      string
		wantValid bool
	}{
		{
			name:      "disabled ignores a bad allowlist pattern",
			cadvisor:  config.CAdvisorConfig{Enabled: false, MetricAllowlist: []string{"("}},
			wantValid: true,
		},
		{
			name:     "bad allowlist pattern",
			cadvisor: config.CAdvisorConfig{Enabled: true, MetricAllowlist: []string{"("}},
			want:     "metric_allowlist[0] is not a valid regular expression",
		},
		{
			name:     "bad scheme",
			cadvisor: config.CAdvisorConfig{Enabled: true, Scheme: "ftp"},
			want:     "scheme must be http or https",
		},
		{
			name:     "path without leading slash",
			cadvisor: config.CAdvisorConfig{Enabled: true, Path: "metrics/cadvisor"},
			want:     "path must start with /",
		},
		{
			name:     "port out of range",
			cadvisor: config.CAdvisorConfig{Enabled: true, Port: "99999"},
			want:     "port must be a TCP port number",
		},
		{
			name: "insecure_skip_verify with ca_file set",
			cadvisor: config.CAdvisorConfig{
				Enabled: true,
				TLS:     config.TLSEndpointConfig{InsecureSkipVerify: true, CAFile: "/ca.crt"},
			},
			want: "tls sets both insecure_skip_verify and ca_file",
		},
		{
			// Interval is written, Timeout is not: ApplyDefaults (whether run by
			// normalize beforehand, or never run at all, as ValidateForTest does
			// here) fills the omitted Timeout with the 10s default, which is not
			// below the 5s interval the operator actually wrote. A three-term
			// `Timeout > 0 && Interval > 0 && Timeout >= Interval` check would
			// miss this because Timeout is still its zero value when validate
			// runs directly -- it must compare EFFECTIVE values instead.
			name:     "defaulted timeout not below a short interval",
			cadvisor: config.CAdvisorConfig{Enabled: true, Interval: 5 * time.Second},
			want:     "timeout must be below interval",
		},
		{
			// Neither field is written: the defaults are 30s/10s, which satisfy
			// the invariant on their own.
			name:      "unset interval and timeout use valid defaults",
			cadvisor:  config.CAdvisorConfig{Enabled: true},
			wantValid: true,
		},
		{
			name:      "enabled with only the required field set is valid",
			cadvisor:  config.CAdvisorConfig{Enabled: true},
			wantValid: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Collect.Metrics.Enabled = true
			// A benign rule so every case exercises only the CAdvisor arm of
			// validate(), not the separate "must define rules/endpoints/cadvisor"
			// guard -- that guard's cadvisor-only branch has its own test below.
			cfg.Collect.Metrics.Rules = []config.MetricsNamespaceRule{{Resources: []string{"pods"}}}
			cfg.Collect.Metrics.CAdvisor = tc.cadvisor

			violations := config.ValidateForTest(cfg)
			if tc.wantValid {
				if len(violations) != 0 {
					t.Fatalf("violations = %v, want none", violations)
				}
				return
			}
			if !containsSubstring(violations, tc.want) {
				t.Fatalf("violations = %v, want one containing %q", violations, tc.want)
			}
		})
	}
}

// A cAdvisor-only configuration -- no rules, no custom endpoints -- must not
// be rejected by the "must define ..." guard: cAdvisor is a metrics source in
// its own right, matching ConfigFromRoot's IsEnabled, which counts a
// cadvisor-only DynamicTargets template as enabled.
func TestCAdvisorOnlyConfigurationIsValid(t *testing.T) {
	cfg := &config.Config{}
	cfg.Collect.Metrics.Enabled = true
	cfg.Collect.Metrics.CAdvisor = config.CAdvisorConfig{Enabled: true}

	if violations := config.ValidateForTest(cfg); len(violations) != 0 {
		t.Fatalf("violations = %v, want none for a cadvisor-only configuration", violations)
	}
}

// A kube-state-metrics-only configuration -- no rules, no custom endpoints,
// no cAdvisor -- must not be rejected by the "must define ..." guard either,
// for the same reason cAdvisor-only is exempt: it is a metrics source in its
// own right, and the early return would otherwise stop it from ever reaching
// KubeStateMetricsConfig.validate.
func TestKubeStateMetricsOnlyConfigurationIsValid(t *testing.T) {
	cfg := &config.Config{}
	cfg.Collect.Metrics.Enabled = true
	cfg.Collect.Metrics.KubeStateMetrics = config.KubeStateMetricsConfig{Enabled: true}

	if violations := config.ValidateForTest(cfg); len(violations) != 0 {
		t.Fatalf("violations = %v, want none for a kube-state-metrics-only configuration", violations)
	}
}

// Metrics enabled with nothing configured at all -- no rules, no custom
// endpoints, cadvisor and kube_state_metrics left at their zero values
// (disabled) -- must still be rejected: there is no metrics source to run.
func TestMetricsEnabledWithNoSourceIsInvalid(t *testing.T) {
	cfg := &config.Config{}
	cfg.Collect.Metrics.Enabled = true

	violations := config.ValidateForTest(cfg)
	if !containsSubstring(violations, "must define rules, custom_endpoints, cadvisor, and/or kube_state_metrics") {
		t.Fatalf("violations = %v, want the must-define-a-source violation", violations)
	}
}

func containsSubstring(violations []string, want string) bool {
	for _, v := range violations {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}

// TestKubeStateMetricsValidationUsesItsOwnDefaults is finding 4: ApplyDefaults
// filled Interval 60s / Timeout 20s while validate fell back to the
// custom-endpoint defaults, 30s / 10s. Load happened to be safe because
// Normalize runs ApplyDefaults first -- but ValidateForTest bypasses Normalize,
// and that is the entry point every validation test in this file uses. So a
// test asserting this exact refusal would have gone GREEN while pinning the
// opposite of what Load does.
func TestKubeStateMetricsValidationUsesItsOwnDefaults(t *testing.T) {
	cases := []struct {
		name      string
		ks        config.KubeStateMetricsConfig
		want      string
		wantValid bool
	}{
		{
			// The case the old fallbacks got backwards. 15s interval with no
			// timeout runs with the 20s kube-state default, which is ABOVE the
			// interval -- refused. Judged against the 10s custom-endpoint
			// default it would have passed, and the agent would then hold a
			// scraper goroutine past every tick.
			name:      "defaulted timeout above a 15s interval is refused",
			ks:        config.KubeStateMetricsConfig{Enabled: true, Interval: 15 * time.Second},
			want:      "timeout must be below interval",
			wantValid: false,
		},
		{
			// 25s clears the 20s default. Under the wrong 10s fallback this
			// also passed, so it is the 15s case above that separates them.
			name:      "a 25s interval clears the 20s default",
			ks:        config.KubeStateMetricsConfig{Enabled: true, Interval: 25 * time.Second},
			wantValid: true,
		},
		{
			name:      "neither field written uses 60s/20s, which are valid together",
			ks:        config.KubeStateMetricsConfig{Enabled: true},
			wantValid: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Collect.Metrics.Enabled = true
			cfg.Collect.Metrics.KubeStateMetrics = tc.ks

			violations := config.ValidateForTest(cfg)
			if tc.wantValid {
				if len(violations) != 0 {
					t.Fatalf("violations = %v, want none", violations)
				}
				return
			}
			if !containsSubstring(violations, tc.want) {
				t.Fatalf("violations = %v, want one containing %q", violations, tc.want)
			}
		})
	}

	// The exported constants must be what ApplyDefaults actually writes, or
	// validate is once again judging against numbers nothing runs with.
	ks := config.KubeStateMetricsConfig{Enabled: true}
	ks.ApplyDefaults()
	if ks.Interval != config.DefaultKubeStateInterval {
		t.Errorf("ApplyDefaults Interval = %v, want %v", ks.Interval, config.DefaultKubeStateInterval)
	}
	if ks.Timeout != config.DefaultKubeStateTimeout {
		t.Errorf("ApplyDefaults Timeout = %v, want %v", ks.Timeout, config.DefaultKubeStateTimeout)
	}
	if config.DefaultKubeStateTimeout == config.DefaultScrapeTimeout {
		t.Error("the kube-state defaults are the custom-endpoint ones again; this test now proves nothing")
	}
}

// The violation must name the value the operator never wrote. Load runs
// Normalize first, so post-normalize both fields are filled and the old
// guarded note never fired -- the operator who wrote only `interval: 15s` was
// told a 20s timeout was wrong with no hint where 20s came from.
func TestTheTimeoutViolationNamesTheDefaultedValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Collect.Metrics.Enabled = true
	cfg.Collect.Metrics.KubeStateMetrics = config.KubeStateMetricsConfig{
		Enabled: true, Interval: 15 * time.Second,
	}
	// Normalize first: this is the path Load takes, and the one where the note
	// used to go silent.
	cfg.Normalize()

	violations := config.ValidateForTest(cfg)
	if !containsSubstring(violations, "effective timeout 20s") {
		t.Errorf("violations = %v, want the effective timeout named", violations)
	}
	if !containsSubstring(violations, "an unset timeout defaults to 20s") {
		t.Errorf("violations = %v, want the message to say where 20s came from", violations)
	}
}

// TestMaxSamplesPerScrapeSeparatesUnsetFromExplicitZero is finding 3.
// normalize rewrote 0 to 20,000 before validation and the collector read the
// normalized value, so the escape hatch values.yaml, both config comments and
// applySampleBudget's own contract all promise -- "0 disables the cap" -- was
// unreachable. A tenant with a legitimately wide allowlist set 0, kept the
// 20,000 cap, and went on silently losing families.
func TestMaxSamplesPerScrapeSeparatesUnsetFromExplicitZero(t *testing.T) {
	t.Run("unset defaults to 20000", func(t *testing.T) {
		cfg := config.Default()
		cfg.Collect.Metrics.Enabled = true
		cfg.Collect.Metrics.MaxSamplesPerScrape = nil
		cfg.Normalize()

		if cfg.Collect.Metrics.MaxSamplesPerScrape == nil {
			t.Fatal("MaxSamplesPerScrape is still nil after Normalize")
		}
		if got := *cfg.Collect.Metrics.MaxSamplesPerScrape; got != config.DefaultMaxSamplesPerScrape {
			t.Fatalf("MaxSamplesPerScrape = %d, want %d", got, config.DefaultMaxSamplesPerScrape)
		}
	})

	t.Run("explicit zero survives Normalize", func(t *testing.T) {
		zero := 0
		cfg := config.Default()
		cfg.Collect.Metrics.Enabled = true
		cfg.Collect.Metrics.MaxSamplesPerScrape = &zero
		cfg.Normalize()

		if cfg.Collect.Metrics.MaxSamplesPerScrape == nil {
			t.Fatal("Normalize dropped the explicit 0")
		}
		if got := *cfg.Collect.Metrics.MaxSamplesPerScrape; got != 0 {
			t.Fatalf("MaxSamplesPerScrape = %d, want the operator's 0 kept: it is the "+
				"documented way to disable the cap", got)
		}
	})

	t.Run("negative is refused", func(t *testing.T) {
		neg := -1
		cfg := &config.Config{}
		cfg.Collect.Metrics.Enabled = true
		cfg.Collect.Metrics.Rules = []config.MetricsNamespaceRule{{Resources: []string{"pods"}}}
		cfg.Collect.Metrics.MaxSamplesPerScrape = &neg

		violations := config.ValidateForTest(cfg)
		if !containsSubstring(violations, "max_samples_per_scrape must not be negative") {
			t.Fatalf("violations = %v, want the negative refusal", violations)
		}
		// The message has to say what the two legitimate values are, or the
		// operator's only next move is to guess.
		if !containsSubstring(violations, "set it to 0 to disable the cap") {
			t.Errorf("violations = %v, want the message to name the escape hatch", violations)
		}
	})
}

// scrape_kind is agent-owned: it is copied verbatim from extra_labels onto
// every sample, so an operator writing it labels their own app's series as
// another source's. Before the Kind field it also re-filed this endpoint's
// scrape health under whatever row they named.
func TestCustomEndpointRejectsTheReservedScrapeKindLabel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Collect.Metrics.Enabled = true
	cfg.Collect.Metrics.CustomEndpoints = []config.MetricEndpointConfig{{
		Name:        "app",
		URL:         "http://app.svc:8080/metrics",
		ExtraLabels: map[string]string{config.ReservedScrapeKindLabel: "cadvisor"},
	}}

	violations := config.ValidateForTest(cfg)
	if !containsSubstring(violations, "extra_labels.scrape_kind is reserved") {
		t.Fatalf("violations = %v, want scrape_kind refused as reserved", violations)
	}

	// Any other label is still an operator's business.
	cfg.Collect.Metrics.CustomEndpoints[0].ExtraLabels = map[string]string{"service": "app"}
	if violations := config.ValidateForTest(cfg); len(violations) != 0 {
		t.Fatalf("violations = %v, want none for an ordinary extra label", violations)
	}
}
