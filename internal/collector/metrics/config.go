package metrics

import (
	"time"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

const (
	defaultPodInterval  = 30 * time.Second
	defaultNodeInterval = 30 * time.Second
	// defaultScrapeInterval and defaultScrapeTimeout mirror pkg/config's
	// exported defaults rather than restating the literals: validation there
	// has to know the EFFECTIVE timeout/interval a config will run with, so
	// the two packages share one source of truth instead of two literals
	// that can drift apart.
	defaultScrapeInterval = pkgconfig.DefaultScrapeInterval
	defaultScrapeTimeout  = pkgconfig.DefaultScrapeTimeout
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
	// DynamicTargets are scrape targets generated from cluster state. They are
	// kept separate from CustomTargets because they are not stable across a
	// process's life: their set changes as nodes join and leave.
	DynamicTargets DynamicTargetsConfig
	// KubeState is a single scrape target whose presence is probed rather than
	// assumed. It is not a CustomTarget because a CustomTarget that cannot be
	// reached reports as broken, and an absent kube-state-metrics is not
	// broken -- it was never installed.
	KubeState    KubeStateTarget
	WriteTimeout time.Duration
	// MaxSamplesPerScrape caps the samples one scrape of one target may
	// publish before it is recorded and published. 0 disables the cap. See
	// pkgconfig.MetricsCollectConfig.MaxSamplesPerScrape for why this is the
	// agent's own advisory ceiling rather than the enforcing one.
	MaxSamplesPerScrape int
}

// KubeStateTarget is the resolved kube-state-metrics scrape.
type KubeStateTarget struct {
	Enabled bool
	// ProbeService is false when an explicit URL was configured: there is no
	// Service to look for, so absence cannot be distinguished from
	// unreachability and the honest report is the connection error.
	ProbeService     bool
	ServiceNamespace string
	ServiceName      string
	ProbeInterval    time.Duration
	Target           ScrapeTarget
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
		WriteTimeout:        defaultWriteTimeout,
		MaxSamplesPerScrape: mc.MaxSamplesPerScrape,
	}
	for _, ep := range mc.CustomEndpoints {
		cfg.CustomTargets = append(cfg.CustomTargets, ScrapeTarget{
			Name:            ep.Name,
			URL:             ep.URL,
			Interval:        ep.Interval,
			Timeout:         ep.Timeout,
			Labels:          copyStringMap(ep.ExtraLabels),
			BearerTokenPath: ep.BearerTokenPath,
			TLSConfig: TLSConfig{
				InsecureSkipVerify: ep.TLS.InsecureSkipVerify,
				CAFile:             ep.TLS.CAFile,
			},
			MetricAllowlist: append([]string(nil), ep.MetricAllowlist...),
			MetricDenylist:  append([]string(nil), ep.MetricDenylist...),
		})
	}
	if mc.CAdvisor.Enabled {
		ca := mc.CAdvisor
		ca.ApplyDefaults()
		cfg.DynamicTargets.RefreshInterval = ca.RefreshInterval
		cfg.DynamicTargets.Templates = append(cfg.DynamicTargets.Templates, TargetTemplate{
			Kind:       "cadvisor",
			NamePrefix: "cadvisor",
			Scheme:     ca.Scheme,
			Port:       ca.Port,
			Path:       ca.Path,
			Interval:   ca.Interval,
			Timeout:    ca.Timeout,
			// scrape_kind travels onto every sample so the consumer's writer
			// and the explorer can tell a cAdvisor series from a
			// kube-state-metrics one without pattern-matching the name.
			Labels:          map[string]string{"scrape_kind": "cadvisor"},
			BearerTokenPath: ca.BearerTokenPath,
			TLSConfig: TLSConfig{
				InsecureSkipVerify: ca.TLS.InsecureSkipVerify,
				CAFile:             ca.TLS.CAFile,
			},
			MetricAllowlist: append([]string(nil), ca.MetricAllowlist...),
			MetricDenylist:  append([]string(nil), ca.MetricDenylist...),
		})
	}
	if mc.KubeStateMetrics.Enabled {
		ks := mc.KubeStateMetrics
		ks.ApplyDefaults()
		cfg.KubeState = KubeStateTarget{
			Enabled:          true,
			ProbeService:     ks.URL == "",
			ServiceNamespace: ks.ServiceNamespace,
			ServiceName:      ks.ServiceName,
			ProbeInterval:    ks.ProbeInterval,
			Target: ScrapeTarget{
				Name:            "kube-state-metrics",
				URL:             ks.ResolvedURL(),
				Interval:        ks.Interval,
				Timeout:         ks.Timeout,
				Labels:          map[string]string{"scrape_kind": "kube_state_metrics"},
				MetricAllowlist: append([]string(nil), ks.MetricAllowlist...),
				MetricDenylist:  append([]string(nil), ks.MetricDenylist...),
			},
		}
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
	if c.KubeState.Enabled {
		if c.KubeState.Target.Interval <= 0 {
			c.KubeState.Target.Interval = defaultScrapeInterval
		}
		if c.KubeState.Target.Timeout <= 0 {
			c.KubeState.Target.Timeout = defaultScrapeTimeout
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
	return len(c.KubernetesMetrics.Rules) > 0 ||
		len(c.CustomTargets) > 0 ||
		len(c.DynamicTargets.Templates) > 0 ||
		c.KubeState.Enabled
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
