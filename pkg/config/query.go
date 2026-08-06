package config

import (
	"fmt"
	"strings"

	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// QueryConfig governs live, on-demand resource reads requested by the Kubexa
// platform, as distinct from collect.state which governs continuous streaming.
//
// Every field is optional and inherits from collect.state when unset.
// Inheritance is per-field: a query section that sets only redact_secrets
// still takes its rules from collect.state.rules. Enabled and RedactSecrets
// are pointers precisely so "unset" stays distinguishable from "false".
type QueryConfig struct {
	// Enabled false refuses every query. Unset means true.
	Enabled *bool `yaml:"enabled,omitempty"`
	// RedactSecrets strips Secret data/stringData from live responses. Unset
	// inherits collect.state.redact_secrets.
	//
	// It is separate from that flag because the two answer different
	// questions: the collect flag means "do not put my Secret values in your
	// database", this one means "do not show my Secret values on screen". An
	// owner may reasonably want values visible live but never persisted.
	RedactSecrets *bool `yaml:"redact_secrets,omitempty"`
	// Rules lists what may be queried. Unset inherits collect.state.rules.
	Rules []QueryRule `yaml:"rules,omitempty"`
}

// QueryRule permits a set of live reads. It is a superset of
// StateNamespaceRule, which is what makes inheriting those rules well-defined.
type QueryRule struct {
	// ID names the rule in logs and errors; optional.
	ID string `yaml:"id,omitempty"`
	// Namespace limits the rule to one namespace. Empty matches every
	// namespace AND cluster-scoped resources. Supports a trailing "*".
	Namespace string `yaml:"namespace,omitempty"`
	// Resources names the permitted resources: a built-in alias ("pods"), or
	// group/version/resource for CRDs.
	Resources []string `yaml:"resources"`
	// Names limits the rule to matching object names. Empty matches all.
	// Supports a trailing "*".
	Names []string `yaml:"names,omitempty"`
	// Verbs is a subset of {list, get}. Empty means both.
	Verbs []string `yaml:"verbs,omitempty"`
	// LabelSelector and FieldSelector are ANDed with whatever the request
	// carries, so a rule can never be widened by the requester.
	LabelSelector string `yaml:"label_selector,omitempty"`
	FieldSelector string `yaml:"field_selector,omitempty"`
}

// QueryEnabled reports whether live queries are answered at all.
func (c *Config) QueryEnabled() bool {
	if c == nil || c.Query.Enabled == nil {
		return true
	}
	return *c.Query.Enabled
}

// QueryRedactSecrets reports whether Secret values are stripped from live
// responses, falling back to collect.state.redact_secrets when unset.
func (c *Config) QueryRedactSecrets() bool {
	if c == nil {
		return false
	}
	if c.Query.RedactSecrets != nil {
		return *c.Query.RedactSecrets
	}
	return c.Collect.State.RedactSecrets
}

// metricsUsageRuleID names the implicit rules below, so a test and an
// operator reading a denial log can both tell them apart from anything the
// owner wrote. Mirrored rules append the source rule's own label so multiple
// mirrors are still distinguishable from one another.
const metricsUsageRuleID = "kubexa-usage-metrics"

// ResourceWildcard is the one entry a query rule's Resources may carry that is
// not a resource name: it permits every group/version/resource, CRDs included.
//
// It exists only here, on the query path. collect.state, collect.logs and
// collect.metrics rules drive one informer or one scrape loop per resource and
// need a concrete list to open; a live query is request-driven, so a wildcard
// there means only "do not check the requested resource against a list".
//
// The spelling is the bare "*" and nothing else. A partial form like "apps/*"
// is not supported and never should be: parseGVR reads a two-part entry as
// version/resource, so "apps/*" and "v1/pods" are the same shape and "apps"
// would be taken for a version.
const ResourceWildcard = "*"

// hasWildcard reports whether a rule permits every resource.
func hasWildcard(resources []string) bool {
	for _, name := range resources {
		if strings.TrimSpace(name) == ResourceWildcard {
			return true
		}
	}
	return false
}

// corePodsGVR and coreNodesGVR are what a rule's Resources entries must
// resolve to (via k8sresource.Parse, which understands every alias -- "pods",
// "pod", "v1/pods") for that rule to be mirrored into a metrics.k8s.io grant.
var (
	corePodsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	coreNodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
)

// permitsListing reports whether a rule's Verbs grants "list" -- empty means
// both list and get are granted, matching how policy.Compile interprets it.
func permitsListing(verbs []string) bool {
	if len(verbs) == 0 {
		return true
	}
	for _, v := range verbs {
		if strings.EqualFold(strings.TrimSpace(v), "list") {
			return true
		}
	}
	return false
}

// ruleTargets reports whether a rule targets want -- either because one of its
// Resources entries resolves to it, or because the rule carries the wildcard
// and so targets everything. Entries that fail to parse are skipped rather
// than treated as a match or an error: a rule that is otherwise fine should
// not lose its metrics mirror over an unrelated typo elsewhere in the same
// Resources list. This is safe to do quietly because it is not the last word
// -- policy.Compile walks the same effective rule set (query.rules, or the
// inherited collect.state.rules) and hard-fails on an unparseable resource,
// and cmd/agent/main.go treats that as fatal, so a genuinely malformed entry
// still stops the agent rather than being silently ignored.
func ruleTargets(resources []string, want schema.GroupVersionResource) bool {
	if hasWildcard(resources) {
		return true
	}
	for _, name := range resources {
		d, err := k8sresource.Parse(name)
		if err != nil {
			continue
		}
		if d.GVR == want {
			return true
		}
	}
	return false
}

// metricsUsageRules mirrors each rule in the effective set that permits
// listing pods or nodes into a matching metrics.k8s.io grant, so the live
// listing's cpu/memory columns are scoped to exactly what the owner already
// granted for the objects themselves -- never more.
//
// It is appended by QueryRules rather than shipped in the chart's query.rules
// because query.rules being non-empty CANCELS the collect.state inheritance
// below: a chart that wrote rules into query.rules would narrow every
// cluster's live reads to metrics alone and break the object listing itself.
//
// A rule that does not grant "list" (Verbs: [get] only, no list implied)
// produces nothing: the object listing it would decorate cannot happen
// either, so there is nothing for a metrics column to attach to.
//
// A rule carrying a FieldSelector is skipped entirely rather than mirrored
// without it. Measured against a live cluster: metrics-server's PodMetrics
// endpoint accepts fieldSelector only on metadata.name and metadata.namespace
// and answers a hard 400 ("is not a known field selector") for anything
// else, including a plain pod field like spec.nodeName -- so a field
// selector copied across is not merely unreliable, it fails the request
// outright. Dropping just the selector instead of the whole rule would
// silently widen the owner's scope, so losing the two metrics columns for
// that rule is the safe failure.
//
// A field-selector rule that permits listing a resource also PERMANENTLY
// blocks mirroring any later rule for that same resource, even one with no
// field selector of its own. This mirrors a quirk of Decide's own
// first-match-wins semantics: Decide does not consider field selectors when
// choosing which rule answers a query, so a field-selector pods rule still
// captures (and narrows) the object LIST -- it is not skipped there, only
// here. A later, broader pods rule is therefore unreachable for the object
// listing too, and mirroring its metrics grant would answer with more than
// the effective object policy actually allows. Blocking is conservative --
// it can cost the columns in cases where the two rules' namespaces do not
// even overlap -- and that is the accepted failure direction.
//
// LabelSelector IS mirrored: measured against a live cluster, PodMetrics
// objects carry the pod's labels and metrics-server's labelSelector query
// param genuinely filters them, and the executor ANDs the policy's selector
// into whatever the request carries, so it can only narrow what comes back,
// never widen it.
func metricsUsageRules(rules []QueryRule) []QueryRule {
	var out []QueryRule
	var blockedPods, blockedNodes bool
	for i, r := range rules {
		if !permitsListing(r.Verbs) {
			continue
		}
		hitPods := ruleTargets(r.Resources, corePodsGVR)
		hitNodes := ruleTargets(r.Resources, coreNodesGVR)

		if r.FieldSelector != "" {
			blockedPods = blockedPods || hitPods
			blockedNodes = blockedNodes || hitNodes
			continue
		}

		if hasWildcard(r.Resources) {
			// The rule permits metrics.k8s.io/v1beta1/pods and /nodes
			// directly, and owner rules are evaluated before every appended
			// mirror, so a mirror of this rule could never be reached.
			//
			// Later rules deliberately keep theirs: a namespace-scoped
			// wildcard leaves other namespaces to the rules that name them,
			// and those rules are reachable. Nothing is blocked here -- the
			// blocking above exists for field selectors, which change which
			// rule answers; a wildcard does not.
			continue
		}

		source := r.ID
		if source == "" {
			source = fmt.Sprintf("#%d", i)
		}
		if hitPods && !blockedPods {
			out = append(out, QueryRule{
				ID:            metricsUsageRuleID + ":" + source + ":pods",
				Namespace:     r.Namespace,
				Resources:     []string{"metrics.k8s.io/v1beta1/pods"},
				Names:         append([]string(nil), r.Names...),
				Verbs:         []string{"list"},
				LabelSelector: r.LabelSelector,
			})
		}
		if hitNodes && !blockedNodes {
			out = append(out, QueryRule{
				ID:            metricsUsageRuleID + ":" + source + ":nodes",
				Namespace:     r.Namespace,
				Resources:     []string{"metrics.k8s.io/v1beta1/nodes"},
				Names:         append([]string(nil), r.Names...),
				Verbs:         []string{"list"},
				LabelSelector: r.LabelSelector,
			})
		}
	}
	return out
}

// QueryRules returns the effective rule set: query.rules when present,
// otherwise collect.state.rules converted rule for rule -- plus, in both
// cases, a metrics.k8s.io mirror of every rule that permits listing pods or
// nodes, so live queries can answer the cpu/memory columns without widening
// what the owner already granted.
//
// An inherited rule carries no Verbs, which means both list and get -- the
// same access the streaming path already had to those resources. Widening is
// not happening here; the owner already agreed this data may leave.
func (c *Config) QueryRules() []QueryRule {
	if c == nil {
		return nil
	}
	if len(c.Query.Rules) > 0 {
		out := make([]QueryRule, len(c.Query.Rules))
		copy(out, c.Query.Rules)
		return append(out, metricsUsageRules(c.Query.Rules)...)
	}
	base := make([]QueryRule, 0, len(c.Collect.State.Rules))
	for _, r := range c.Collect.State.Rules {
		base = append(base, QueryRule{
			ID:            r.ID,
			Namespace:     r.Namespace,
			Resources:     append([]string(nil), r.Resources...),
			LabelSelector: r.LabelSelector,
			FieldSelector: r.FieldSelector,
		})
	}
	return append(base, metricsUsageRules(base)...)
}

// validateQuery returns one violation string per problem, matching the
// aggregation the other collect sections use so Validate can report every
// config error in one pass.
func (c *Config) validateQuery() []string {
	if c == nil {
		return nil
	}
	var violations []string
	for i, rule := range c.Query.Rules {
		label := rule.ID
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		if len(rule.Resources) == 0 {
			violations = append(violations, fmt.Sprintf("query rule %s: at least one resource is required", label))
			continue
		}
		for _, name := range rule.Resources {
			trimmed := strings.TrimSpace(name)
			if trimmed == ResourceWildcard {
				continue
			}
			// k8sresource.Parse tolerates a partial wildcard like "apps/*" or
			// "apps/v1/*" -- parseGVR reads it as an ordinary two- or
			// three-part GVR whose Resource field happens to be "*", and
			// returns it with a nil error. That compiles into a rule that
			// matches no real GVR, which is the exact failure
			// validatePattern's comment calls out: a policy the owner
			// believes is in force and is not. Only the bare "*" is a
			// wildcard; reject every other form containing one.
			if strings.Contains(trimmed, ResourceWildcard) {
				violations = append(violations, fmt.Sprintf(
					"query rule %s: %q is not a supported resource: only the bare %q is a wildcard, not a partial form",
					label, name, ResourceWildcard))
				continue
			}
			if _, err := k8sresource.Parse(name); err != nil {
				violations = append(violations, fmt.Sprintf("query rule %s: %v", label, err))
			}
		}
		if err := validatePattern(rule.Namespace); err != nil {
			violations = append(violations, fmt.Sprintf("query rule %s: namespace %q: %v", label, rule.Namespace, err))
		}
		for _, n := range rule.Names {
			if err := validatePattern(n); err != nil {
				violations = append(violations, fmt.Sprintf("query rule %s: name %q: %v", label, n, err))
			}
		}
		for _, v := range rule.Verbs {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "list", "get":
			default:
				violations = append(violations, fmt.Sprintf("query rule %s: unsupported verb %q (want list or get)", label, v))
			}
		}
	}

	// QueryRules inherits collect.state.rules verbatim when query.rules is
	// empty, but StateCollectConfig.validate returns early -- and skips its
	// own rules entirely -- whenever collect.state.enabled is false. Without
	// this, a wildcard sitting in a disabled state section's rules would
	// never be checked by anything: not collect.state's own validation
	// (skipped, disabled), not the loop above (it walks c.Query.Rules, which
	// is empty here). It would reach Compile unvalidated and compile into a
	// real, live grant. The wildcard is a query.rules-only spelling -- reject
	// it on the way in, regardless of collect.state's enabled/disabled state.
	if len(c.Query.Rules) == 0 {
		for i, rule := range c.Collect.State.Rules {
			label := rule.ID
			if label == "" {
				label = fmt.Sprintf("#%d", i)
			}
			for _, name := range rule.Resources {
				if strings.TrimSpace(name) == ResourceWildcard {
					violations = append(violations, fmt.Sprintf(
						"collect.state.rules %s: %q is not supported here; the wildcard is a query.rules-only spelling",
						label, name))
				}
			}
		}
	}
	return violations
}

// ValidateQuery reports the query section's problems as a single error.
// Callers that only need a yes/no answer use this; Config.Validate uses
// validateQuery directly so query violations join the same aggregated
// *ValidationError as every other section's.
func (c *Config) ValidateQuery() error {
	violations := c.validateQuery()
	if len(violations) == 0 {
		return nil
	}
	return &ValidationError{Violations: violations}
}

// validatePattern enforces trailing-"*" prefix matching, the same shape
// pod_names and node_names already use elsewhere in this config.
//
// A pattern with "*" anywhere else is rejected rather than silently treated as
// a literal: a rule that can never match is a policy the owner believes is in
// force but is not, which is the worst possible failure for a security gate.
func validatePattern(p string) error {
	idx := strings.Index(p, "*")
	if idx == -1 || idx == len(p)-1 {
		return nil
	}
	return fmt.Errorf("%q is not a supported pattern: \"*\" is allowed only as a trailing wildcard", p)
}
