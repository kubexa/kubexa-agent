package config

import (
	"fmt"
	"strings"

	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
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

// metricsUsageRuleID names the implicit rule below, so a test and an operator
// reading a denial log can both tell it apart from anything the owner wrote.
const metricsUsageRuleID = "kubexa-usage-metrics"

// metricsUsageRule permits reading metrics.k8s.io, which is where a live
// listing's cpu and memory columns come from.
//
// It is appended by QueryRules rather than shipped in the chart's query.rules
// because query.rules being non-empty CANCELS the collect.state inheritance
// below: a chart that wrote this rule into query.rules would narrow every
// cluster's live reads to metrics alone and break the object listing itself.
//
// List only, and every namespace: the caller reads a page of objects, and
// nodes are cluster-scoped. metrics.k8s.io returns two numbers per object and
// nothing else, so this opens no surface that listing the objects did not
// already open.
func metricsUsageRule() QueryRule {
	return QueryRule{
		ID: metricsUsageRuleID,
		Resources: []string{
			"metrics.k8s.io/v1beta1/pods",
			"metrics.k8s.io/v1beta1/nodes",
		},
		Verbs: []string{"list"},
	}
}

// QueryRules returns the effective rule set: query.rules when present,
// otherwise collect.state.rules converted rule for rule -- plus the implicit
// metrics usage rule in both cases, so live queries can always answer the
// cpu/memory columns.
//
// An inherited rule carries no Verbs, which means both list and get -- the
// same access the streaming path already had to those resources. Widening is
// not happening here; the owner already agreed this data may leave.
func (c *Config) QueryRules() []QueryRule {
	if c == nil {
		return nil
	}
	if len(c.Query.Rules) > 0 {
		out := make([]QueryRule, len(c.Query.Rules), len(c.Query.Rules)+1)
		copy(out, c.Query.Rules)
		return append(out, metricsUsageRule())
	}
	out := make([]QueryRule, 0, len(c.Collect.State.Rules)+1)
	for _, r := range c.Collect.State.Rules {
		out = append(out, QueryRule{
			ID:            r.ID,
			Namespace:     r.Namespace,
			Resources:     append([]string(nil), r.Resources...),
			LabelSelector: r.LabelSelector,
			FieldSelector: r.FieldSelector,
		})
	}
	return append(out, metricsUsageRule())
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
