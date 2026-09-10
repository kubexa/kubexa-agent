// Package policy decides whether a mutation is permitted by the cluster
// owner's agent configuration.
//
// It is a separate package from internal/query/policy on purpose. One
// compiled rule type cannot hold two opposite meanings for an empty verb
// list -- empty means "list and get" there and "nothing" here -- and a
// merged type eventually lets one path read the other's rule.
package policy

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

// Verb is the operation a mutation performs. The set is closed.
type Verb string

const (
	VerbPatch   Verb = "patch"
	VerbDelete  Verb = "delete"
	VerbRestart Verb = "restart"
	VerbScale   Verb = "scale"
	VerbCreate  Verb = "create"
)

// Ref names one API resource the way the dynamic client addresses it.
type Ref struct {
	Group    string // "" for the core group
	Version  string
	Resource string // plural
}

// Decision is the outcome of evaluating one mutation against the policy.
type Decision struct {
	Allowed bool
	RuleID  string // the authorizing rule, for the audit trail and for logs
	Reason  string // empty when allowed
}

// Policy is an immutable, compiled rule set. It is built once at startup and
// never mutated: the agent has no config hot-reload, and a policy that can
// change at runtime is a policy nobody can reason about.
type Policy struct {
	enabled bool
	rules   []compiledRule
}

type compiledRule struct {
	id           string
	namespace    string
	namespaceSet bool
	resources    []Ref
	names        []string
	verbs        map[Verb]bool
}

// Compile builds a Policy from the agent's root configuration.
func Compile(root *pkgconfig.Config) (*Policy, error) {
	if root == nil {
		return &Policy{}, nil
	}

	rules := root.MutateRules()
	compiled := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		label := r.ID
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		refs := make([]Ref, 0, len(r.Resources))
		for _, name := range r.Resources {
			trimmed := strings.TrimSpace(name)
			// Difference from the query policy, #3: there is no wildcard
			// branch at all. Config validation (pkg/config/mutate.go)
			// already refuses every "*" form, bare or partial, in
			// mutate.rules[].resources -- a write path has no use for "every
			// resource this token names", so matchesResource below compares
			// parsed GVRs by equality only.
			if trimmed == pkgconfig.ResourceWildcard || strings.Contains(trimmed, pkgconfig.ResourceWildcard) {
				return nil, fmt.Errorf("mutate rule %s: %q is not a supported resource: no wildcard form is permitted on a write path",
					label, name)
			}
			d, err := k8sresource.Parse(trimmed)
			if err != nil {
				return nil, fmt.Errorf("mutate rule %s: %w", label, err)
			}
			// Parse tolerates a malformed GVR by returning a zero Descriptor
			// rather than an error (see k8sresource.descriptorForGVR). A
			// zero Ref would match nothing, so reject it here instead of
			// shipping a rule the owner believes is in force.
			if d.GVR.Resource == "" || d.GVR.Version == "" {
				return nil, fmt.Errorf("mutate rule %s: unsupported resource %q", label, name)
			}
			refs = append(refs, Ref{
				Group:    d.GVR.Group,
				Version:  d.GVR.Version,
				Resource: d.GVR.Resource,
			})
		}

		// Difference from the query policy, #2: an empty verb list grants
		// NOTHING here, the opposite of QueryRule.Verbs (where empty means
		// "list and get"). Config validation already refuses a rule with no
		// verbs (pkg/config/mutate.go), so this is the second line of
		// defence -- a programmatically-built config.Config must not be able
		// to bypass it and acquire a write it never named.
		verbs := make(map[Verb]bool, len(r.Verbs))
		for _, v := range r.Verbs {
			verbs[Verb(strings.ToLower(strings.TrimSpace(v)))] = true
		}

		compiled = append(compiled, compiledRule{
			id:           label,
			namespace:    r.Namespace,
			namespaceSet: r.Namespace != "",
			resources:    refs,
			names:        append([]string(nil), r.Names...),
			verbs:        verbs,
		})
	}

	return &Policy{
		// Difference from the query policy, #1: enabled is false unless
		// MutateEnabled() is explicitly true. QueryConfig treats an unset
		// Enabled as "yes"; MutateConfig treats it as "no" on purpose (Task
		// 2), so there is no "unset means yes" branch to mirror here.
		enabled: root.MutateEnabled(),
		rules:   compiled,
	}, nil
}

// Decide evaluates one mutation.
//
// Rules are evaluated in configuration order and the FIRST match decides the
// whole outcome. Because no rule can deny, first-match-wins gives the same
// allow/deny answer as treating the rules as additive; the ordering exists so
// that when two rules match, it is unambiguous which rule authorized it.
func (p *Policy) Decide(ref Ref, verb Verb, namespace, name string) Decision {
	if p == nil || !p.enabled {
		return Decision{Reason: "mutation is disabled in this agent's configuration"}
	}
	if err := validateRef(ref); err != nil {
		return Decision{Reason: err.Error()}
	}
	for _, r := range p.rules {
		if !r.matchesResource(ref) {
			continue
		}
		if !r.matchesNamespace(namespace) {
			continue
		}
		if name != "" && !matchesPattern(name, r.names) {
			continue
		}
		if !r.verbs[verb] {
			continue
		}
		return Decision{
			Allowed: true,
			RuleID:  r.id,
		}
	}
	return Decision{Reason: fmt.Sprintf(
		"the cluster owner's agent configuration does not permit %s on %s in namespace %q",
		verb, refString(ref), namespace)}
}

// validateRef rejects a ref whose segments cannot name a real Kubernetes
// resource. It runs before any rule is consulted, so no rule can permit one.
//
// See internal/query/policy.validateRef for the disclosure this closes: the
// dynamic client builds its URL with path.Join, which does not validate the
// resource segment, so an unchecked ref can reach a real endpoint the caller
// never named.
func validateRef(ref Ref) error {
	if errs := validation.IsDNS1123Label(ref.Resource); len(errs) > 0 {
		return fmt.Errorf("resource %q is not a valid Kubernetes resource name: %s",
			ref.Resource, errs[0])
	}
	if errs := validation.IsDNS1123Label(ref.Version); len(errs) > 0 {
		return fmt.Errorf("version %q is not a valid Kubernetes API version: %s",
			ref.Version, errs[0])
	}
	// The core group is spelled as the empty string, which is not a subdomain.
	if ref.Group != "" {
		if errs := validation.IsDNS1123Subdomain(ref.Group); len(errs) > 0 {
			return fmt.Errorf("group %q is not a valid Kubernetes API group: %s",
				ref.Group, errs[0])
		}
	}
	return nil
}

func (r compiledRule) matchesResource(ref Ref) bool {
	for _, got := range r.resources {
		if got == ref {
			return true
		}
	}
	return false
}

// matchesNamespace implements the cluster-scoped rule from the design: a
// request with no namespace (a cluster-scoped write) is granted only by a
// rule that itself names no namespace. An owner who wrote "namespace: stage"
// never agreed to let a cluster-scoped object be mutated.
func (r compiledRule) matchesNamespace(ns string) bool {
	if !r.namespaceSet {
		return true
	}
	if ns == "" {
		return false
	}
	return matchesPattern(ns, []string{r.namespace})
}

// matchesPattern implements trailing-"*" prefix matching, the same semantics
// internal/query/policy.MatchesName uses. An empty pattern list matches
// everything.
func matchesPattern(value string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(value, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if value == p {
			return true
		}
	}
	return false
}

func refString(ref Ref) string {
	if ref.Group == "" {
		return ref.Version + "/" + ref.Resource
	}
	return ref.Group + "/" + ref.Version + "/" + ref.Resource
}

// MatchesName reports whether an object name satisfies a rule's name
// patterns. It mirrors internal/query/policy.MatchesName's semantics and, for
// the same reason, is a package function over patterns rather than a method
// that re-walks the rule list.
func MatchesName(name string, patterns []string) bool {
	return matchesPattern(name, patterns)
}
