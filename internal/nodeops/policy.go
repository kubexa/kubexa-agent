package nodeops

import (
	"errors"
	"fmt"
	"strings"

	mutatepolicy "github.com/kubexa/kubexa-agent/internal/mutate/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// Verb is one node operation.
type Verb string

const (
	VerbCordon   Verb = "cordon"
	VerbUncordon Verb = "uncordon"
	VerbDrain    Verb = "drain"
)

// Decision is the policy's answer for one (node, verb).
type Decision struct {
	Allowed bool
	// RuleID names the mutate.node.nodes[i] pattern that matched.
	RuleID string
	// Reason names the check that refused: disabled, node, or verb.
	Reason string
}

// Policy is mutate.node compiled. Every method is nil-safe and denies.
type Policy struct {
	enabled  bool
	patterns []string
	verbs    map[Verb]bool
}

// Compile validates mutate.node UNCONDITIONALLY (see
// config.ValidateNodeMutateRules) and compiles it. A disabled section with
// an invalid rule is still an error here: the caller decides whether that is
// fatal (enabled) or a warning (disabled), as cmd/agent/main.go does for
// mutate.rules.
func Compile(root *config.Config) (*Policy, error) {
	if root == nil {
		return &Policy{}, nil
	}
	if violations := config.ValidateNodeMutateRules(root.Mutate.Node); len(violations) > 0 {
		return nil, errors.New("mutate.node: " + strings.Join(violations, "; "))
	}
	s := root.MutateNodeSettings()
	verbs := make(map[Verb]bool, len(s.Verbs))
	for _, v := range s.Verbs {
		verbs[Verb(strings.ToLower(v))] = true
	}
	return &Policy{enabled: root.MutateNodeEnabled(), patterns: s.Nodes, verbs: verbs}, nil
}

// Decide checks disabled, node pattern and verb, in that order, and names
// the first that refuses. The verb is checked AFTER the node so the reason
// for a refused job on a node the owner never listed does not leak which
// verbs the owner granted.
func (p *Policy) Decide(node string, verb Verb) Decision {
	if p == nil || !p.enabled {
		return Decision{Reason: "node operations are disabled in this agent's configuration (mutate.node.enabled)"}
	}
	rule := ""
	for i, pat := range p.patterns {
		if mutatepolicy.MatchesName(node, []string{pat}) {
			rule = fmt.Sprintf("mutate.node.nodes[%d]", i)
			break
		}
	}
	if rule == "" {
		return Decision{Reason: fmt.Sprintf("node %q matches no mutate.node.nodes pattern", node)}
	}
	if !p.verbs[verb] {
		return Decision{Reason: fmt.Sprintf("verb %q is not granted by mutate.node.verbs", verb)}
	}
	return Decision{Allowed: true, RuleID: rule}
}

// AllowsAnyNode reports whether at least one node could be operated on.
func (p *Policy) AllowsAnyNode() bool {
	return p != nil && p.enabled && len(p.patterns) > 0 && len(p.verbs) > 0
}

// VerbFromProto maps the wire enum. UNSPECIFIED and unknown values are
// refused: the zero value of a verb dispatched to a cluster is not a thing
// that should exist.
func VerbFromProto(v agentv1.NodeJobVerb) (Verb, bool) {
	switch v {
	case agentv1.NodeJobVerb_NODE_JOB_VERB_CORDON:
		return VerbCordon, true
	case agentv1.NodeJobVerb_NODE_JOB_VERB_UNCORDON:
		return VerbUncordon, true
	case agentv1.NodeJobVerb_NODE_JOB_VERB_DRAIN:
		return VerbDrain, true
	default:
		return "", false
	}
}
