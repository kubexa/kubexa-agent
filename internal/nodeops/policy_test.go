package nodeops

import (
	"strings"
	"testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func on() *bool { b := true; return &b }

func cfgWith(n config.NodeMutateConfig) *config.Config {
	return &config.Config{Mutate: config.MutateConfig{Node: n}}
}

func TestCompileValidatesEvenWhenDisabled(t *testing.T) {
	_, err := Compile(cfgWith(config.NodeMutateConfig{Verbs: []string{"nope"}}))
	if err == nil || !strings.Contains(err.Error(), `unknown verb "nope"`) {
		t.Fatalf("Compile = %v, want the verb violation", err)
	}
}

func TestDecideOrder(t *testing.T) {
	disabled, _ := Compile(cfgWith(config.NodeMutateConfig{Nodes: []string{"*"}, Verbs: []string{"drain"}}))
	if d := disabled.Decide("w1", VerbDrain); d.Allowed || !strings.Contains(d.Reason, "disabled") {
		t.Fatalf("disabled: %+v", d)
	}
	var nilPolicy *Policy
	if d := nilPolicy.Decide("w1", VerbDrain); d.Allowed {
		t.Fatalf("nil policy allowed: %+v", d)
	}

	p, err := Compile(cfgWith(config.NodeMutateConfig{Enabled: on(), Nodes: []string{"aks-*", "exact"}, Verbs: []string{"cordon", "Drain"}}))
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Decide("gke-1", VerbCordon); d.Allowed || !strings.Contains(d.Reason, "matches no mutate.node.nodes pattern") {
		t.Fatalf("unmatched node: %+v", d)
	}
	if d := p.Decide("aks-1", VerbUncordon); d.Allowed || !strings.Contains(d.Reason, `verb "uncordon" is not granted`) {
		t.Fatalf("ungranted verb: %+v", d)
	}
	if d := p.Decide("aks-1", VerbDrain); !d.Allowed || d.RuleID != "mutate.node.nodes[0]" {
		t.Fatalf("granted drain (case-insensitive verb): %+v", d)
	}
	if d := p.Decide("exact", VerbCordon); !d.Allowed || d.RuleID != "mutate.node.nodes[1]" {
		t.Fatalf("exact node: %+v", d)
	}
	if !p.AllowsAnyNode() {
		t.Fatal("AllowsAnyNode false with two patterns")
	}
}

func TestEmptyNodesMatchesNothing(t *testing.T) {
	p, err := Compile(cfgWith(config.NodeMutateConfig{Enabled: on(), Verbs: []string{"cordon"}}))
	if err != nil {
		t.Fatal(err)
	}
	if p.AllowsAnyNode() {
		t.Fatal("AllowsAnyNode true with no pattern")
	}
	if d := p.Decide("anything", VerbCordon); d.Allowed {
		t.Fatalf("empty nodes matched: %+v", d)
	}
}

func TestVerbFromProto(t *testing.T) {
	for proto, want := range map[agentv1.NodeJobVerb]Verb{
		agentv1.NodeJobVerb_NODE_JOB_VERB_CORDON:   VerbCordon,
		agentv1.NodeJobVerb_NODE_JOB_VERB_UNCORDON: VerbUncordon,
		agentv1.NodeJobVerb_NODE_JOB_VERB_DRAIN:    VerbDrain,
	} {
		if got, ok := VerbFromProto(proto); !ok || got != want {
			t.Fatalf("%v -> %q/%v", proto, got, ok)
		}
	}
	if _, ok := VerbFromProto(agentv1.NodeJobVerb_NODE_JOB_VERB_UNSPECIFIED); ok {
		t.Fatal("UNSPECIFIED mapped to a verb")
	}
	if _, ok := VerbFromProto(agentv1.NodeJobVerb(99)); ok {
		t.Fatal("unknown enum mapped to a verb")
	}
}
