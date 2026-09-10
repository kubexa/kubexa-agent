package config

import (
	"fmt"
	"strings"

	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

// MutateConfig governs write operations the Kubexa platform may perform on
// this cluster. It is a THIRD gate alongside Kubernetes RBAC and the query
// policy, and it shares nothing with either.
//
// Nothing here inherits from collect.state or from query. Inheritance is
// defensible between two read paths; into a write path it is the mechanism by
// which a config silently gains a verb its author never wrote.
type MutateConfig struct {
	// Enabled false refuses every mutation. Unset means FALSE -- the
	// opposite of QueryConfig.Enabled, and deliberately so: an operator who
	// upgrades the agent and writes nothing must not acquire a write path.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Rules lists what may be changed. There is no inheritance and no
	// implicit rule: an empty list with Enabled true permits nothing.
	Rules []MutateRule `yaml:"rules,omitempty"`
}

// MutateRule permits a set of writes.
type MutateRule struct {
	ID        string   `yaml:"id,omitempty"`
	Namespace string   `yaml:"namespace,omitempty"` // empty matches all; trailing "*" supported
	Resources []string `yaml:"resources"`           // no wildcard; see validateMutateConfig
	Names     []string `yaml:"names,omitempty"`     // empty matches all; trailing "*" supported
	// Verbs is a subset of {patch, delete, restart, scale, create}. EMPTY
	// GRANTS NOTHING and is refused at load. QueryRule.Verbs means "both"
	// when empty; copying that convention here would turn a rule whose
	// author wrote no verbs into a delete grant.
	Verbs []string `yaml:"verbs"`
}

// MutateVerbs is the closed set a rule may name.
var MutateVerbs = []string{"patch", "delete", "restart", "scale", "create"}

// MutateEnabled reports whether any mutation is answered at all.
func (c *Config) MutateEnabled() bool {
	if c == nil || c.Mutate.Enabled == nil {
		return false
	}
	return *c.Mutate.Enabled
}

// MutateRules returns the configured rules. There is no inheritance.
func (c *Config) MutateRules() []MutateRule {
	if c == nil {
		return nil
	}
	return c.Mutate.Rules
}

func (c *Config) validateMutate() []string {
	if c == nil || !c.MutateEnabled() {
		return nil
	}
	return validateMutateConfig(c.Mutate)
}

func validateMutateConfig(m MutateConfig) []string {
	var errs []string
	for i, r := range m.Rules {
		prefix := fmt.Sprintf("mutate.rules[%d]", i)
		if len(r.Resources) == 0 {
			errs = append(errs, prefix+".resources must not be empty")
		}
		for _, name := range r.Resources {
			trimmed := strings.TrimSpace(name)
			if trimmed == ResourceWildcard {
				errs = append(errs, prefix+
					`.resources: the "*" wildcard is not permitted on a write path;`+
					" name every resource explicitly")
				continue
			}
			// k8sresource.Parse tolerates a partial wildcard like "apps/*" or
			// "apps/v1/*" -- it reads as an ordinary two- or three-part GVR
			// whose Resource field happens to be "*", and returns it with a
			// nil error. That compiles into a rule that matches no real GVR:
			// a policy the owner believes is in force and is not. Reject
			// every form containing "*", not just the bare one.
			if strings.Contains(trimmed, ResourceWildcard) {
				errs = append(errs, fmt.Sprintf(
					"%s.resources: %q is not a supported resource: no wildcard form is permitted on a write path",
					prefix, name))
				continue
			}
			if _, err := k8sresource.Parse(trimmed); err != nil {
				errs = append(errs, fmt.Sprintf("%s.resources: %v", prefix, err))
			}
		}
		if len(r.Verbs) == 0 {
			errs = append(errs, prefix+
				".verbs must name at least one of "+strings.Join(MutateVerbs, ", ")+
				" (unlike query rules, an empty list grants nothing)")
		}
		for _, v := range r.Verbs {
			if !knownMutateVerb(v) {
				errs = append(errs, fmt.Sprintf("%s.verbs: unknown verb %q; want one of %s",
					prefix, v, strings.Join(MutateVerbs, ", ")))
			}
		}
		if err := validatePattern(r.Namespace); r.Namespace != "" && err != nil {
			errs = append(errs, fmt.Sprintf("%s.namespace: %v", prefix, err))
		}
		// validatePatterns (collect.go) compiles each entry as a regular
		// expression -- it serves log/state rules. Query rules validate
		// Names with validatePattern (singular, query.go), the trailing-"*"
		// glob check, which is the semantic the mutate policy also
		// implements. Use the same per-name loop query.go's own validator
		// uses, not validatePatterns.
		for _, n := range r.Names {
			if err := validatePattern(n); err != nil {
				errs = append(errs, fmt.Sprintf("%s.names: %q: %v", prefix, n, err))
			}
		}
	}
	return errs
}

func knownMutateVerb(v string) bool {
	for _, known := range MutateVerbs {
		if strings.EqualFold(strings.TrimSpace(v), known) {
			return true
		}
	}
	return false
}
