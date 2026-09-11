package policy

import "testing"

// White-box, package policy (not policy_test): pins Decide's own invariant --
// a compiled rule with no granted verbs must refuse every verb -- independent
// of whatever Compile chooses to validate.
//
// Compile currently refuses to produce a *Policy at all for a config rule
// with no verbs (it calls config.ValidateMutateRules unconditionally, before
// enabled is even read), which makes an "empty verbs means every verb"
// regression in Decide or in compiledRule construction unreachable through
// the public Compile API today. That argument holds only as long as
// Compile's validation stays unconditional; if a later change relaxes it,
// the pathway reopens. This test does not go through Compile at all -- it
// builds the compiledRule directly, the same way a bug in Compile's rule
// construction (not just its validation) could one day produce one -- so it
// keeps catching the regression regardless of what Compile does.
func TestDecideRefusesEveryVerbWhenRuleGrantsNone(t *testing.T) {
	p := &Policy{
		enabled: true,
		rules: []compiledRule{
			{
				id:        "no-verbs",
				resources: []Ref{{Version: "v1", Resource: "pods"}},
				// verbs is intentionally left at its zero value (nil map):
				// the shape Compile would never hand back today, and the
				// shape this test exists to keep refused if it ever could.
			},
		},
	}
	ref := Ref{Version: "v1", Resource: "pods"}
	for _, v := range []Verb{VerbPatch, VerbDelete, VerbRestart, VerbScale, VerbCreate} {
		if d := p.Decide(ref, v, "dev", "web-0"); d.Allowed {
			t.Fatalf("verb %q must be refused when the rule grants no verbs, got allowed via rule %q", v, d.RuleID)
		}
	}
}
