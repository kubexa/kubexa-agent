// Package policy decides whether the cluster owner's agent configuration
// permits a console session in a container. It is a separate package from
// internal/mutate/policy and internal/query/policy: the rule shape differs
// (a container dimension, no verbs, no resources) and a shared compiled type
// would eventually let one path read another's rule.
package policy

import (
	"fmt"
	"strings"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

// Decision is the outcome of evaluating one target against the policy.
type Decision struct {
	Allowed bool
	RuleID  string
	Reason  string
}

// Policy is an immutable compiled rule set, built once at startup.
type Policy struct {
	enabled bool
	rules   []compiledRule
}

type compiledRule struct {
	id         string
	namespace  string
	names      []string
	containers []string
}

// Compile builds a Policy. Rules are validated UNCONDITIONALLY, before
// enabled is read, for the reason mutate/policy.Compile gives: a section
// disabled today is enabled tomorrow by an operator who was told the config
// was valid.
func Compile(root *pkgconfig.Config) (*Policy, error) {
	if root == nil {
		return &Policy{}, nil
	}
	rules := root.ExecPodRules()
	if v := pkgconfig.ValidatePodExecRules(rules); len(v) > 0 {
		return nil, fmt.Errorf("exec.pod rules: %s", strings.Join(v, "; "))
	}
	p := &Policy{enabled: root.ExecPodEnabled()}
	for i, r := range rules {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			id = fmt.Sprintf("exec.pod.rules[%d]", i)
		}
		p.rules = append(p.rules, compiledRule{
			id:         id,
			namespace:  strings.TrimSpace(r.Namespace),
			names:      trimAll(r.Names),
			containers: trimAll(r.Containers),
		})
	}
	return p, nil
}

// Decide evaluates one target. Rules are evaluated in configuration order
// and the first match decides, so the authorizing rule id is unambiguous.
// The container is consulted for every rule, empty included: a rule that
// restricts containers refuses an empty name rather than treating it as a
// match-all (the executor resolves the default container BEFORE calling
// Decide, so a legitimate request never arrives empty).
func (p *Policy) Decide(namespace, pod, container string) Decision {
	if p == nil || !p.enabled {
		return Decision{Reason: "pod console is disabled in this agent's configuration"}
	}
	for _, r := range p.rules {
		if r.namespace != "" && !matchesPattern(namespace, []string{r.namespace}) {
			continue
		}
		if !matchesPattern(pod, r.names) {
			continue
		}
		if !matchesPattern(container, r.containers) {
			continue
		}
		return Decision{Allowed: true, RuleID: r.id}
	}
	return Decision{Reason: fmt.Sprintf(
		"the cluster owner's agent configuration does not permit a console in %s/%s container %q",
		namespace, pod, container)}
}

// AllowsAnyPod answers the capability catalog's coarse question: could a
// console ever be permitted here. Decide stays authoritative per request.
func (p *Policy) AllowsAnyPod() bool {
	return p != nil && p.enabled && len(p.rules) > 0
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// matchesPattern is trailing-"*" prefix matching, the semantics
// mutate/policy and query/policy use. An empty pattern list matches all.
func matchesPattern(value string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pat := range patterns {
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(value, strings.TrimSuffix(pat, "*")) {
				return true
			}
			continue
		}
		if value == pat {
			return true
		}
	}
	return false
}

// NodePolicy decides whether a node console may open on a node. It lives
// beside Policy rather than in its own package: there is no inverted
// default to keep apart (both sections mean "no rule, no shell"), and
// matchesPattern is exactly what it needs.
type NodePolicy struct {
	enabled  bool
	patterns []string
}

// CompileNode builds a NodePolicy. Patterns are validated UNCONDITIONALLY,
// before enabled is read, for the reason Compile gives.
func CompileNode(root *pkgconfig.Config) (*NodePolicy, error) {
	if root == nil {
		return &NodePolicy{}, nil
	}
	patterns := root.ExecNodeRules()
	if v := pkgconfig.ValidateNodeExecRules(patterns); len(v) > 0 {
		return nil, fmt.Errorf("exec.node rules: %s", strings.Join(v, "; "))
	}
	return &NodePolicy{enabled: root.ExecNodeEnabled(), patterns: trimAll(patterns)}, nil
}

// Decide evaluates one node name. The first matching pattern decides and
// names the rule as exec.node.nodes[i]. An empty pattern list matches
// nothing: unlike a pod rule's fields, "no nodes" is not "all nodes" (see
// NodeExecConfig.Nodes).
func (p *NodePolicy) Decide(node string) Decision {
	if p == nil || !p.enabled {
		return Decision{Reason: "node console is disabled in this agent's configuration"}
	}
	for i, pat := range p.patterns {
		if matchesPattern(node, []string{pat}) {
			return Decision{Allowed: true, RuleID: fmt.Sprintf("exec.node.nodes[%d]", i)}
		}
	}
	return Decision{Reason: fmt.Sprintf("node %q matches no exec.node.nodes pattern", node)}
}

// AllowsAnyNode reports whether at least one node could be entered.
func (p *NodePolicy) AllowsAnyNode() bool {
	return p != nil && p.enabled && len(p.patterns) > 0
}
