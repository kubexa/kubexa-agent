package metrics

import (
	"time"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

const (
	defaultPodInterval    = 30 * time.Second
	defaultNodeInterval   = 30 * time.Second
	defaultScrapeInterval = 30 * time.Second
	defaultScrapeTimeout  = 10 * time.Second
	defaultWriteTimeout   = 100 * time.Millisecond
)

// TLSConfig configures TLS for custom scrape targets.
type TLSConfig struct {
	InsecureSkipVerify bool
	CAFile             string
}

// KubeMetricsRule defines a scoped Kubernetes Metrics API scrape rule.
type KubeMetricsRule struct {
	ID            string
	Namespace     string
	PodNames      []string
	NodeNames     []string
	LabelSelector string
	FieldSelector string
	CollectPods   bool
	CollectNodes  bool
	PodInterval   time.Duration
	NodeInterval  time.Duration
}

// KubernetesMetricsConfig configures scraping from metrics.k8s.io.
type KubernetesMetricsConfig struct {
	PodInterval  time.Duration
	NodeInterval time.Duration
	Rules        []KubeMetricsRule
}

// ScrapeTarget defines a custom Prometheus exposition endpoint.
type ScrapeTarget struct {
	Name            string
	URL             string
	Interval        time.Duration
	Timeout         time.Duration
	Labels          map[string]string
	BearerTokenPath string
	TLSConfig       TLSConfig
	MetricAllowlist []string
	MetricDenylist  []string
}

// Config holds runtime settings for the metrics scraper.
type Config struct {
	Enabled           bool
	KubernetesMetrics KubernetesMetricsConfig
	CustomTargets     []ScrapeTarget
	WriteTimeout      time.Duration
}

// DefaultConfig returns documented defaults for the metrics scraper.
func DefaultConfig() Config {
	return Config{
		Enabled: true,
		KubernetesMetrics: KubernetesMetricsConfig{
			PodInterval:  defaultPodInterval,
			NodeInterval: defaultNodeInterval,
			Rules: []KubeMetricsRule{
				{CollectPods: true, CollectNodes: true},
			},
		},
		WriteTimeout: defaultWriteTimeout,
	}
}

// ConfigFromRoot maps the agent root configuration into collector settings.
func ConfigFromRoot(root *pkgconfig.Config) Config {
	if root == nil {
		return DefaultConfig()
	}
	mc := root.Collect.Metrics
	cfg := Config{
		Enabled: mc.Enabled,
		KubernetesMetrics: KubernetesMetricsConfig{
			PodInterval:  mc.PodInterval,
			NodeInterval: mc.NodeInterval,
			Rules:        kubeRulesFromRoot(mc),
		},
		WriteTimeout: defaultWriteTimeout,
	}
	for _, ep := range mc.CustomEndpoints {
		cfg.CustomTargets = append(cfg.CustomTargets, ScrapeTarget{
			Name:     ep.Name,
			URL:      ep.URL,
			Interval: ep.Interval,
			Labels:   copyStringMap(ep.ExtraLabels),
		})
	}
	cfg.ApplyDefaults()
	return cfg
}

func kubeRulesFromRoot(mc pkgconfig.MetricsCollectConfig) []KubeMetricsRule {
	if len(mc.Rules) == 0 {
		return nil
	}
	out := make([]KubeMetricsRule, 0, len(mc.Rules))
	for _, rule := range mc.Rules {
		out = append(out, KubeMetricsRule{
			ID:            rule.ID,
			Namespace:     rule.Namespace,
			PodNames:      append([]string(nil), rule.PodNames...),
			NodeNames:     append([]string(nil), rule.NodeNames...),
			LabelSelector: rule.EffectiveLabelSelector(),
			FieldSelector: rule.FieldSelector,
			CollectPods:   rule.IncludesPods(),
			CollectNodes:  rule.IncludesNodes(),
			PodInterval:   rule.ResolvePodInterval(mc.PodInterval),
			NodeInterval:  rule.ResolveNodeInterval(mc.NodeInterval),
		})
	}
	return out
}

// ApplyDefaults fills zero values with documented defaults.
func (c *Config) ApplyDefaults() {
	if c == nil {
		return
	}
	if c.KubernetesMetrics.PodInterval <= 0 {
		c.KubernetesMetrics.PodInterval = defaultPodInterval
	}
	if c.KubernetesMetrics.NodeInterval <= 0 {
		c.KubernetesMetrics.NodeInterval = defaultNodeInterval
	}
	for i := range c.KubernetesMetrics.Rules {
		if c.KubernetesMetrics.Rules[i].PodInterval <= 0 {
			c.KubernetesMetrics.Rules[i].PodInterval = c.KubernetesMetrics.PodInterval
		}
		if c.KubernetesMetrics.Rules[i].NodeInterval <= 0 {
			c.KubernetesMetrics.Rules[i].NodeInterval = c.KubernetesMetrics.NodeInterval
		}
	}
	for i := range c.CustomTargets {
		if c.CustomTargets[i].Interval <= 0 {
			c.CustomTargets[i].Interval = defaultScrapeInterval
		}
		if c.CustomTargets[i].Timeout <= 0 {
			c.CustomTargets[i].Timeout = defaultScrapeTimeout
		}
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaultWriteTimeout
	}
}

// IsEnabled reports whether metrics collection is active.
func (c *Config) IsEnabled() bool {
	if c == nil || !c.Enabled {
		return false
	}
	return len(c.KubernetesMetrics.Rules) > 0 || len(c.CustomTargets) > 0
}

// HasKubeMetricsRules reports whether any Kubernetes Metrics API rules are configured.
func (c *Config) HasKubeMetricsRules() bool {
	return c != nil && len(c.KubernetesMetrics.Rules) > 0
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
