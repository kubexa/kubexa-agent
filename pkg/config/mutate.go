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
	// Node is the `mutate.node` section: cordon / uncordon / drain. It
	// shares NOTHING with Enabled or Rules above -- a `resources: [nodes]`
	// rule with `patch` still permits a raw patch of spec.unschedulable
	// through the mutation path, but it does not grant `cordon` here, and
	// Enabled false above does not switch this section off. Two paths, two
	// grants.
	Node NodeMutateConfig `yaml:"node,omitempty"`
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
	return ValidateMutateRules(m.Rules)
}

// ValidateMutateRules validates each rule in isolation. It does NOT consult
// MutateConfig.Enabled -- unlike validateMutate, which short-circuits to nil
// for a disabled section because that is the right behaviour at config-load
// time (see its comment). This function exists for a caller that must not
// inherit that short-circuit: internal/mutate/policy.Compile calls it
// unconditionally, because a section that is disabled today is enabled
// tomorrow by an operator who was told the config was valid, and the rules
// it validates today are the rules that take effect then.
func ValidateMutateRules(rules []MutateRule) []string {
	var errs []string
	for i, r := range rules {
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

// NodeMutateConfig governs the node jobs (cordon, uncordon, drain) the
// platform may run on this cluster.
type NodeMutateConfig struct {
	// Enabled false refuses every node job. Unset means FALSE.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Nodes lists node-name patterns (trailing "*" supported). Empty
	// matches NOTHING; unlike exec.node it is not a load error, because an
	// operator who enables the section and names no node has permitted
	// nothing, which is safe -- the agent logs it once at boot.
	Nodes []string `yaml:"nodes,omitempty"`
	// Verbs is a subset of {cordon, uncordon, drain}. EMPTY GRANTS NOTHING
	// and is refused at load, the mutate.rules convention.
	Verbs []string `yaml:"verbs,omitempty"`
	// MaxTimeoutSec caps a drain's DrainOptions.timeout_sec. 0 means the
	// default (1800); otherwise [30, 86400].
	MaxTimeoutSec int `yaml:"max_timeout_sec,omitempty"`
}

// NodeMutateVerbs is the closed set mutate.node.verbs may name.
var NodeMutateVerbs = []string{"cordon", "uncordon", "drain"}

const defaultNodeMutateMaxTimeoutSec = 1800

// NodeMutateSettings is mutate.node with its defaults applied.
type NodeMutateSettings struct {
	Nodes         []string
	Verbs         []string
	MaxTimeoutSec int
}

// MutateNodeEnabled reports whether any node job is answered at all. It
// reads mutate.node.enabled ONLY -- not mutate.enabled.
func (c *Config) MutateNodeEnabled() bool {
	if c == nil || c.Mutate.Node.Enabled == nil {
		return false
	}
	return *c.Mutate.Node.Enabled
}

// MutateNodeSettings returns mutate.node with defaults applied.
func (c *Config) MutateNodeSettings() NodeMutateSettings {
	s := NodeMutateSettings{MaxTimeoutSec: defaultNodeMutateMaxTimeoutSec}
	if c == nil {
		return s
	}
	s.Nodes = trimAll(c.Mutate.Node.Nodes)
	s.Verbs = trimAll(c.Mutate.Node.Verbs)
	if c.Mutate.Node.MaxTimeoutSec != 0 {
		s.MaxTimeoutSec = c.Mutate.Node.MaxTimeoutSec
	}
	return s
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (c *Config) validateMutateNode() []string {
	if c == nil || !c.MutateNodeEnabled() {
		return nil
	}
	return ValidateNodeMutateRules(c.Mutate.Node)
}

// ValidateNodeMutateRules validates mutate.node in isolation and does NOT
// consult Enabled, for the reason ValidateMutateRules gives: the policy
// compiler calls it unconditionally.
func ValidateNodeMutateRules(n NodeMutateConfig) []string {
	var errs []string
	for _, p := range n.Nodes {
		if err := validatePattern(p); err != nil {
			errs = append(errs, fmt.Sprintf("mutate.node.nodes: %q: %v", p, err))
		}
	}
	if len(n.Verbs) == 0 {
		errs = append(errs, "mutate.node.verbs must name at least one of "+strings.Join(NodeMutateVerbs, ", ")+
			" (an empty list grants nothing)")
	}
	for _, v := range n.Verbs {
		if !knownNodeMutateVerb(v) {
			errs = append(errs, fmt.Sprintf("mutate.node.verbs: unknown verb %q; want one of %s",
				v, strings.Join(NodeMutateVerbs, ", ")))
		}
	}
	if n.MaxTimeoutSec != 0 && (n.MaxTimeoutSec < 30 || n.MaxTimeoutSec > 86400) {
		errs = append(errs, "mutate.node.max_timeout_sec must be between 30 and 86400")
	}
	return errs
}

func knownNodeMutateVerb(v string) bool {
	for _, known := range NodeMutateVerbs {
		if strings.EqualFold(strings.TrimSpace(v), known) {
			return true
		}
	}
	return false
}
