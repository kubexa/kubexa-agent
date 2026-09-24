package policy_test

import (
	"fmt"
	"testing"

	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

// TestBlockValidationAndPolicyCompileAgree pins config.ValidateBlock (the
// gate the platform's push path hits first) and policy.Compile (the gate
// that actually decides live queries) to the same accept/reject verdict for
// every resource string.
//
// Every one of these passes ApplyBlocks today and fails Compile: validation
// accepts what the real consumer refuses. The test asserts the two AGREE, in
// both directions, so it keeps failing if either side drifts later --
// whichever side turns out to be the lenient one.
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
