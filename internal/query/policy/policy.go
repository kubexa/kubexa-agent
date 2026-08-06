// Package policy decides whether a live resource query is permitted by the
// cluster owner's agent configuration.
//
// This is a second gate, independent of Kubernetes RBAC. RBAC answers "may
// this ServiceAccount read the resource"; this answers "did the cluster owner
// agree that the Kubexa platform may read it". Both must say yes.
package policy

import (
	"fmt"
	"strings"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

// Verb is the operation a query performs. The set is closed: this path is
// read-only and nothing here requests a verb the agent's ClusterRole lacks.
type Verb string

const (
	VerbList Verb = "list"
	VerbGet  Verb = "get"
)

// Ref names one API resource the way the dynamic client addresses it.
type Ref struct {
	Group    string // "" for the core group
	Version  string
	Resource string // plural
}

// Decision is the outcome of evaluating one query against the policy.
type Decision struct {
	Allowed bool
	// RedactSecrets tells the executor whether to strip Secret values.
	RedactSecrets bool
	// LabelSelector and FieldSelector come from the matching rule and are
	// ANDed with whatever the request carried.
	LabelSelector string
	FieldSelector string
	// NamePatterns are the authorizing rule's name patterns. The executor
	// filters LIST rows against these via MatchesName, so row filtering uses
	// the same rule that permitted the query -- see MatchesName's comment for
	// why re-consulting the policy per row would be a data-exposure bug.
	NamePatterns []string
	// Reason explains a denial in terms the operator can act on. Empty when
	// allowed.
	Reason string
}

// Policy is an immutable, compiled rule set. It is built once at startup and
// never mutated: the agent has no config hot-reload, and a policy that can
// change at runtime is a policy nobody can reason about.
type Policy struct {
	enabled       bool
	redactSecrets bool
	rules         []compiledRule
}

type compiledRule struct {
	id            string
	namespace     string
	namespaceSet  bool
	wildcard      bool
	resources     []Ref
	names         []string
	allowList     bool
	allowGet      bool
	labelSelector string
	fieldSelector string
}

// Compile builds a Policy from the agent's root configuration, resolving the
// inheritance from collect.state described in pkg/config/query.go.
func Compile(root *pkgconfig.Config) (*Policy, error) {
	if root == nil {
		return &Policy{}, nil
	}
	if err := root.ValidateQuery(); err != nil {
		return nil, err
	}

	rules := root.QueryRules()
	compiled := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		label := r.ID
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		refs := make([]Ref, 0, len(r.Resources))
		wildcard := false
		for _, name := range r.Resources {
			if strings.TrimSpace(name) == pkgconfig.ResourceWildcard {
				wildcard = true
				continue
			}
			d, err := k8sresource.Parse(name)
			if err != nil {
				return nil, fmt.Errorf("query rule %s: %w", label, err)
			}
			// Parse tolerates a malformed GVR by returning a zero Descriptor
			// rather than an error (see parse.go descriptorForGVR). A zero
			// Ref would match nothing, so reject it here instead of shipping
			// a rule the owner believes is in force.
			if d.GVR.Resource == "" || d.GVR.Version == "" {
				return nil, fmt.Errorf("query rule %s: unsupported resource %q", label, name)
			}
			refs = append(refs, Ref{
				Group:    d.GVR.Group,
				Version:  d.GVR.Version,
				Resource: d.GVR.Resource,
			})
		}

		allowList, allowGet := true, true
		if len(r.Verbs) > 0 {
			allowList, allowGet = false, false
			for _, v := range r.Verbs {
				switch Verb(strings.ToLower(strings.TrimSpace(v))) {
				case VerbList:
					allowList = true
				case VerbGet:
					allowGet = true
				}
			}
		}

		compiled = append(compiled, compiledRule{
			id:            label,
			namespace:     r.Namespace,
			namespaceSet:  r.Namespace != "",
			wildcard:      wildcard,
			resources:     refs,
			names:         append([]string(nil), r.Names...),
			allowList:     allowList,
			allowGet:      allowGet,
			labelSelector: r.LabelSelector,
			fieldSelector: r.FieldSelector,
		})
	}

	return &Policy{
		enabled:       root.QueryEnabled(),
		redactSecrets: root.QueryRedactSecrets(),
		rules:         compiled,
	}, nil
}

// Decide evaluates one query.
//
// Rules are evaluated in configuration order and the FIRST match decides the
// whole outcome. Because no rule can deny, first-match-wins gives the same
// allow/deny answer as treating the rules as additive; the ordering exists so
// that when two rules match, it is unambiguous whose selectors apply.
//
// name is empty for a LIST. A name pattern therefore cannot deny a LIST here;
// the executor applies it to the returned rows instead.
func (p *Policy) Decide(ref Ref, verb Verb, namespace, name string) Decision {
	if p == nil || !p.enabled {
		return Decision{Reason: "live resource query is disabled in this agent's configuration"}
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
		switch verb {
		case VerbList:
			if !r.allowList {
				continue
			}
		case VerbGet:
			if !r.allowGet {
				continue
			}
		default:
			continue
		}
		return Decision{
			Allowed:       true,
			RedactSecrets: p.redactSecrets,
			LabelSelector: r.labelSelector,
			FieldSelector: r.fieldSelector,
			NamePatterns:  r.names,
		}
	}
	return Decision{Reason: fmt.Sprintf(
		"the cluster owner's agent configuration does not permit %s on %s in namespace %q",
		verb, refString(ref), namespace)}
}

// AllowsAnyList reports whether any rule grants list on this resource, in any
// namespace. It answers the catalog's question -- "could a query for this type
// ever succeed" -- and is deliberately coarser than Decide: a per-GVR boolean
// cannot express a namespace-scoped policy, so Decide stays authoritative at
// request time.
func (p *Policy) AllowsAnyList(group, version, resource string) bool {
	return p.allowsAny(Ref{Group: group, Version: version, Resource: resource}, VerbList)
}

// AllowsAnyGet reports whether any rule grants get on this resource, in any
// namespace. See AllowsAnyList for why this is coarser than Decide.
func (p *Policy) AllowsAnyGet(group, version, resource string) bool {
	return p.allowsAny(Ref{Group: group, Version: version, Resource: resource}, VerbGet)
}

func (p *Policy) allowsAny(ref Ref, verb Verb) bool {
	if p == nil || !p.enabled {
		return false
	}
	for _, r := range p.rules {
		if !r.matchesResource(ref) {
			continue
		}
		if verb == VerbList && r.allowList {
			return true
		}
		if verb == VerbGet && r.allowGet {
			return true
		}
	}
	return false
}

func (r compiledRule) matchesResource(ref Ref) bool {
	if r.wildcard {
		return true
	}
	for _, got := range r.resources {
		if got == ref {
			return true
		}
	}
	return false
}

// WildcardRuleIDs names the compiled rules that permit every resource.
//
// It exists for one caller: the startup warning in cmd/agent. A wildcard rule
// covers secrets like everything else, and paired with unredacted Secret
// values that is the widest read policy this agent can hold -- not something an
// operator should first learn from a screen. A disabled policy permits nothing,
// so it reports nothing.
func (p *Policy) WildcardRuleIDs() []string {
	if p == nil || !p.enabled {
		return nil
	}
	var out []string
	for _, r := range p.rules {
		if r.wildcard {
			out = append(out, r.id)
		}
	}
	return out
}

// matchesNamespace implements the cluster-scoped rule from the design: a
// request with no namespace (a cluster-scoped read, or a deliberate
// all-namespaces read) is granted only by a rule that itself names no
// namespace. An owner who wrote "namespace: stage" never agreed to let
// node or persistent-volume data leave the cluster.
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
// pod_names and node_names use in internal/collector/metrics/filter.go.
// An empty pattern list matches everything.
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

// MatchesName reports whether an object name satisfies the name patterns a
// Decision carried out. The executor calls it once per LIST row, because a
// LIST carries no name at decision time.
//
// It is a package function over patterns rather than a Policy method taking a
// namespace, and that is the whole point. A method would walk the rule list a
// second time, and when the namespace it received differed from the one Decide
// was evaluated against it could select a MORE PERMISSIVE rule than the one
// that authorized the query -- admitting a row a direct get is denied. Passing
// the authorizing rule's patterns through the Decision makes one rule
// selection per query, so there is no second selection to diverge.
func MatchesName(name string, patterns []string) bool {
	return matchesPattern(name, patterns)
}
