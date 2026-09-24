package policy_test

import (
	"fmt"
	"testing"

	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

// TestBlockValidationAndPolicyCompileAgree pins that config.ValidateBlock
// (the gate the platform's push path hits first) never accepts a resource
// string that policy.Compile (the gate that actually decides live queries)
// refuses -- the lenient-validator direction, which is what produced the
// "pods/" defect this test was written for.
//
// It cannot pin the other direction: Compile calls root.ValidateQuery() as
// its own first gate, so a validateQuery that turns stricter is inherited by
// Compile, not contradicted by it -- the two would still agree here. That
// direction is caught elsewhere, loudly: any test that compiles a policy
// over ordinary resources, e.g. internal/query's executor suite, fails if
// validateQuery starts rejecting a valid string.
//
// Every one of these passes ApplyBlocks today and fails Compile: validation
// accepts what the real consumer refuses.
func TestBlockValidationAndPolicyCompileAgree(t *testing.T) {
	cases := []string{
		"pods", "apps/v1/deployments", "v1/pods", "*",
		"pods/", "a/", "apps/v1/", "/", "//", "a//c",
	}
	for _, resource := range cases {
		t.Run(resource, func(t *testing.T) {
			body := []byte(fmt.Sprintf("rules:\n  - resources: [%q]\n", resource))
			validateErr := config.ValidateBlock("query", body)

			base, err := config.LoadFromYAML([]byte(
				"agent:\n  tenant_token: t\ngateway:\n  address: gw:443\n",
			))
			if err != nil {
				t.Fatalf("LoadFromYAML base: %v", err)
			}
			cfg, applyErr := config.ApplyBlocks(base, map[string][]byte{"query": body})

			acceptedByValidate := validateErr == nil
			// ApplyBlocks re-runs ValidateBlock internally, so applyErr and
			// validateErr must already agree; the interesting comparison is
			// against Compile, which only runs on a config ApplyBlocks
			// actually produced.
			var compileErr error
			if cfg != nil {
				_, compileErr = policy.Compile(cfg)
			} else {
				compileErr = applyErr
			}
			acceptedByCompile := compileErr == nil

			if acceptedByValidate != acceptedByCompile {
				t.Fatalf("resource %q: ValidateBlock accepted=%v (err=%v), Compile accepted=%v (err=%v) -- the two checks disagree",
					resource, acceptedByValidate, validateErr, acceptedByCompile, compileErr)
			}
		})
	}
}
