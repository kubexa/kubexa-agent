package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// Normalize fills default collection rules and assigns rule IDs where missing.
func (c *Config) Normalize() {
	if c == nil {
		return
	}
	c.Collect.Logs.normalize()
	c.Collect.State.normalize()
}

func (l *LogsCollectConfig) normalize() {
	if l == nil {
		return
	}
	l.CheckpointDir = strings.TrimSpace(l.CheckpointDir)
	l.ExcludeNamespaces = normalizeNamespaceList(l.ExcludeNamespaces)
	if l.Enabled && len(l.Rules) == 0 {
		l.Rules = []LogNamespaceRule{{Namespace: ""}}
	}
	for i := range l.Rules {
		l.Rules[i].normalize(i)
	}
}

func normalizeNamespaceList(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, ns := range names {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *LogNamespaceRule) normalize(index int) {
	if r == nil {
		return
	}
	if r.ID == "" {
		r.ID = fmt.Sprintf("logs-%d", index)
	}
	if r.LabelSelector == "" && len(r.Labels) > 0 {
		r.LabelSelector = labelsToSelector(r.Labels)
	}
}

func (s *StateCollectConfig) normalize() {
	if s == nil {
		return
	}
	if s.Enabled && len(s.Rules) == 0 {
		resources := make([]string, len(defaultStateResources))
		copy(resources, defaultStateResources)
		s.Rules = []StateNamespaceRule{{Resources: resources}}
	}
	for i := range s.Rules {
		s.Rules[i].normalize(i)
	}
}

func (r *StateNamespaceRule) normalize(index int) {
	if r == nil {
		return
	}
	if r.ID == "" {
		r.ID = fmt.Sprintf("state-%d", index)
	}
}

func (l *LogsCollectConfig) validate() []string {
	if l == nil || !l.Enabled {
		return nil
	}
	if len(l.Rules) == 0 {
		return []string{"collect.logs.rules must contain at least one entry when logs collection is enabled"}
	}
	if strings.Contains(l.CheckpointDir, "..") {
		return []string{"collect.logs.checkpoint_dir must not contain .."}
	}
	var violations []string
	seen := make(map[string]struct{}, len(l.Rules))
	for i, rule := range l.Rules {
		violations = append(violations, rule.validate(i)...)
		if rule.ID == "" {
			continue
		}
		if _, dup := seen[rule.ID]; dup {
			violations = append(violations, fmt.Sprintf("collect.logs.rules[%d].id %q is duplicated", i, rule.ID))
		}
		seen[rule.ID] = struct{}{}
	}
	return violations
}

func (r *LogNamespaceRule) validate(index int) []string {
	if r == nil {
		return nil
	}
	prefix := fmt.Sprintf("collect.logs.rules[%d]", index)
	var violations []string
	if r.TailLines != nil && *r.TailLines < 0 {
		violations = append(violations, prefix+".tail_lines must not be negative")
	}
	return violations
}

func (s *StateCollectConfig) validate() []string {
	if s == nil || !s.Enabled {
		return nil
	}
	if len(s.Rules) == 0 {
		return []string{"collect.state.rules must contain at least one entry when state collection is enabled"}
	}
	var violations []string
	seen := make(map[string]struct{}, len(s.Rules))
	for i, rule := range s.Rules {
		violations = append(violations, rule.validate(i)...)
		if rule.ID == "" {
			continue
		}
		if _, dup := seen[rule.ID]; dup {
			violations = append(violations, fmt.Sprintf("collect.state.rules[%d].id %q is duplicated", i, rule.ID))
		}
		seen[rule.ID] = struct{}{}
	}
	return violations
}

func (r *StateNamespaceRule) validate(index int) []string {
	if r == nil {
		return nil
	}
	prefix := fmt.Sprintf("collect.state.rules[%d]", index)
	var violations []string
	if len(r.Resources) == 0 {
		violations = append(violations, prefix+".resources must contain at least one resource")
	}
	for j, resource := range r.Resources {
		if _, err := ParseResourceKind(resource); err != nil {
			violations = append(violations, fmt.Sprintf("%s.resources[%d]: %v", prefix, j, err))
		}
	}
	return violations
}

// EffectiveLabelSelector returns the label selector for the rule, including Labels shorthand.
func (r *LogNamespaceRule) EffectiveLabelSelector() string {
	if r == nil {
		return ""
	}
	if r.LabelSelector != "" {
		return r.LabelSelector
	}
	return labelsToSelector(r.Labels)
}

// ResolveTailLines returns the effective tail line count for the rule.
func (r *LogNamespaceRule) ResolveTailLines(global int64) int64 {
	if r == nil || r.TailLines == nil {
		return global
	}
	return *r.TailLines
}

// ResolveFollow returns whether log streaming should follow new output for the rule.
func (r *LogNamespaceRule) ResolveFollow(global bool) bool {
	if r == nil || r.Follow == nil {
		return global
	}
	return *r.Follow
}

// ListOptions returns Kubernetes list options derived from the rule filters.
func (r *LogNamespaceRule) ListOptions() metav1.ListOptions {
	if r == nil {
		return metav1.ListOptions{}
	}
	return metav1.ListOptions{
		LabelSelector: r.EffectiveLabelSelector(),
		FieldSelector: r.FieldSelector,
	}
}

// ResolveResyncPeriod returns the effective resync period for the rule.
func (r *StateNamespaceRule) ResolveResyncPeriod(global time.Duration) time.Duration {
	if r == nil || r.ResyncPeriod <= 0 {
		return global
	}
	return r.ResyncPeriod
}

// WatchListOptions returns Kubernetes list options for watch operations derived from the rule.
func (r *StateNamespaceRule) WatchListOptions() metav1.ListOptions {
	if r == nil {
		return metav1.ListOptions{}
	}
	return metav1.ListOptions{
		LabelSelector: r.LabelSelector,
		FieldSelector: r.FieldSelector,
	}
}

// ParseResourceKind maps a configured resource name to its proto ResourceKind.
func ParseResourceKind(name string) (agentv1.ResourceKind, error) {
	return k8sresource.ParseResourceKind(name)
}

// ParseStateResource maps a configured resource name to full API metadata.
func ParseStateResource(name string) (k8sresource.Descriptor, error) {
	return k8sresource.Parse(name)
}

// ParseResourceKinds maps configured resource names to proto ResourceKind values.
func ParseResourceKinds(names []string) ([]agentv1.ResourceKind, error) {
	kinds := make([]agentv1.ResourceKind, 0, len(names))
	for _, name := range names {
		kind, err := ParseResourceKind(name)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

// ToProtoLogCollectors converts log collection rules to gateway proto configs.
func (l *LogsCollectConfig) ToProtoLogCollectors() ([]*agentv1.LogCollectorConfig, error) {
	if l == nil || !l.Enabled {
		return nil, nil
	}
	out := make([]*agentv1.LogCollectorConfig, 0, len(l.Rules))
	for _, rule := range l.Rules {
		id := rule.ID
		if id == "" {
			id = uuid.NewString()
		}
		cfg := &agentv1.LogCollectorConfig{
			Id:         id,
			Namespace:  rule.Namespace,
			Containers: append([]string(nil), rule.Containers...),
			Follow:     rule.ResolveFollow(l.Follow),
			TailLines:  int32(rule.ResolveTailLines(l.TailLines)),
		}
		if selector := rule.EffectiveLabelSelector(); selector != "" {
			cfg.PodSelectors = []string{selector}
		}
		out = append(out, cfg)
	}
	return out, nil
}

// ToProtoWatchers converts state collection rules to gateway proto configs.
func (s *StateCollectConfig) ToProtoWatchers() ([]*agentv1.WatcherConfig, error) {
	if s == nil {
		return nil, nil
	}
	out := make([]*agentv1.WatcherConfig, 0, len(s.Rules))
	for _, rule := range s.Rules {
		kinds, err := ParseResourceKinds(rule.Resources)
		if err != nil {
			return nil, fmt.Errorf("state rule %q: %w", rule.ID, err)
		}
		resync := rule.ResolveResyncPeriod(s.ResyncPeriod)
		out = append(out, &agentv1.WatcherConfig{
			Id:        rule.ID,
			Namespace: rule.Namespace,
			Kinds:     kinds,
			ResyncSec: int32(resync / time.Second),
		})
	}
	return out, nil
}

// ToProtoSnapshot builds a ConfigSnapshot from the agent configuration.
func (c *Config) ToProtoSnapshot() (*agentv1.ConfigSnapshot, error) {
	if c == nil {
		return nil, fmt.Errorf("config is nil")
	}
	logCollectors, err := c.Collect.Logs.ToProtoLogCollectors()
	if err != nil {
		return nil, fmt.Errorf("log collectors: %w", err)
	}
	watchers, err := c.Collect.State.ToProtoWatchers()
	if err != nil {
		return nil, fmt.Errorf("watchers: %w", err)
	}
	scrapers := make([]*agentv1.MetricScrapeConfig, 0, len(c.Collect.Metrics.CustomEndpoints))
	for _, ep := range c.Collect.Metrics.CustomEndpoints {
		scrapers = append(scrapers, &agentv1.MetricScrapeConfig{
			Id:          ep.Name,
			Endpoint:    ep.URL,
			IntervalSec: int32(ep.Interval / time.Second),
			ExtraLabels: ep.ExtraLabels,
		})
	}
	return &agentv1.ConfigSnapshot{
		LogCollectors:       logCollectors,
		Watchers:            watchers,
		MetricScrapers:      scrapers,
		BatchSize:           int32(c.Buffer.BatchSize),
		FlushIntervalMs:     int32(c.Buffer.FlushInterval / time.Millisecond),
	}, nil
}

func labelsToSelector(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, ",")
}
