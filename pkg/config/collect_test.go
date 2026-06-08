package config_test

import (
	"strings"
	"testing"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/pkg/config"
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
