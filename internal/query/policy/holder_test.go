package policy_test

import (
	"slices"
	"testing"

	"github.com/kubexa/kubexa-agent/internal/capability"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

// configWithUnredactedWildcard returns a *config.Config whose query.rules
// contains a wildcard rule with redaction off -- the exact shape
// UnredactedWildcardRuleIDs exists to name. Built through config.LoadFromYAML,
// not a struct literal or a bare yaml.Unmarshal, so the fixture cannot drift
// from what a real config produces: LoadFromYAML runs cfg.Validate(), which
// requires agent.tenant_token and gateway.address, so both are included
// alongside the query.rules this helper is actually about.
func configWithUnredactedWildcard(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromYAML([]byte(`
agent:
  tenant_token: test-token
gateway:
  address: gateway.example.com:443
query:
  rules:
    - id: everything
      resources: ["*"]
`))
	if err != nil {
		t.Fatalf("LoadFromYAML: %v", err)
	}
	return cfg
}

func TestSwapReportsTheSameWarningBootReports(t *testing.T) {
	// Boot logs UnredactedWildcardRuleIDs (cmd/agent/main.go:191). If that
	// check stays only at boot, a wildcard rule pushed from the app enters
	// without the warning its existence depends on. One function must
	// produce both, so this test fails the moment the swap path stops
	// calling it.
	wildcard := configWithUnredactedWildcard(t)

	_, bootWarnings, err := policy.NewHolder(wildcard)
	if err != nil {
		t.Fatal(err)
	}
	if len(bootWarnings) == 0 {
		t.Fatal("boot must warn about an unredacted wildcard rule")
	}

	h, _, err := policy.NewHolder(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	swapWarnings, err := h.Swap(wildcard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(swapWarnings, bootWarnings) {
		t.Fatalf("swap warnings %v must equal boot warnings %v", swapWarnings, bootWarnings)
	}
}

func TestCurrentReturnsTheSwappedPolicy(t *testing.T) {
	h, _, err := policy.NewHolder(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	before := h.Current()
	if before == nil {
		t.Fatal("Current() must not be nil after NewHolder")
	}

	wildcard := configWithUnredactedWildcard(t)
	if _, err := h.Swap(wildcard); err != nil {
		t.Fatal(err)
	}
	after := h.Current()
	if after == before {
		t.Fatal("Current() must return the swapped-in policy, not the one from NewHolder")
	}
	if len(after.WildcardRuleIDs()) == 0 {
		t.Fatal("Current() must reflect the swapped config's wildcard rule")
	}
}

// A Holder must satisfy capability.PolicySource -- the capability reporter is
// wired to it directly, not to a Current() snapshot, so the reporter's
// per-GVR verdicts follow a swap instead of freezing at their boot values.
var _ capability.PolicySource = (*policy.Holder)(nil)

func TestHolderAllowsAnyListFollowsASwap(t *testing.T) {
	base, err := config.LoadFromYAML([]byte(`
agent:
  tenant_token: test-token
gateway:
  address: gateway.example.com:443
query:
  rules:
    - id: pods-only
      resources: [pods]
`))
	if err != nil {
		t.Fatalf("LoadFromYAML: %v", err)
	}

	h, _, err := policy.NewHolder(base)
	if err != nil {
		t.Fatal(err)
	}

	var src capability.PolicySource = h
	if src.AllowsAnyList("apps", "v1", "deployments") {
		t.Fatal("deployments must not be allowed before the swap widens the config")
	}

	widened, err := config.LoadFromYAML([]byte(`
agent:
  tenant_token: test-token
gateway:
  address: gateway.example.com:443
query:
  rules:
    - id: pods-and-deployments
      resources: [pods, deployments]
`))
	if err != nil {
		t.Fatalf("LoadFromYAML: %v", err)
	}
	if _, err := h.Swap(widened); err != nil {
		t.Fatal(err)
	}

	if !src.AllowsAnyList("apps", "v1", "deployments") {
		t.Fatal("AllowsAnyList must reflect the swapped-in config through the interface value")
	}
}
