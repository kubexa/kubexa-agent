package state

import (
	"strings"
	"time"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

const (
	defaultWorkerCount  = 4
	defaultBufferSize   = 1000
	defaultWriteTimeout = 100 * time.Millisecond
)

// Config holds runtime settings for the Kubernetes state watcher.
type Config struct {
	Enabled      bool
	Namespaces   []string
	ResyncPeriod time.Duration
	WorkerCount  int
	BufferSize   int
	// Rules carries per-namespace watch options from pkg/config (resources, label/field selectors, resync).
	Rules []pkgconfig.StateNamespaceRule
	// WriteTimeout bounds non-blocking queue writes.
	WriteTimeout time.Duration
}

// DefaultConfig returns documented defaults for the state watcher.
func DefaultConfig() Config {
	return Config{
		Enabled:      true,
		ResyncPeriod: 0,
		WorkerCount:  defaultWorkerCount,
		BufferSize:   defaultBufferSize,
		WriteTimeout: defaultWriteTimeout,
		Rules: []pkgconfig.StateNamespaceRule{
			{Resources: []string{"pods", "services", "deployments", "secrets"}},
		},
	}
}

// ConfigFromRoot maps the agent root configuration into collector settings.
func ConfigFromRoot(root *pkgconfig.Config) Config {
	if root == nil {
		return DefaultConfig()
	}
	sc := root.Collect.State
	cfg := Config{
		Enabled:      sc.Enabled,
		ResyncPeriod: sc.ResyncPeriod,
		Rules:        append([]pkgconfig.StateNamespaceRule(nil), sc.Rules...),
		WriteTimeout: defaultWriteTimeout,
	}
	cfg.Namespaces = namespacesFromRules(sc.Rules)
	cfg.ApplyDefaults()
	return cfg
}

// ApplyDefaults fills zero values with documented defaults.
func (c *Config) ApplyDefaults() {
	if c == nil {
		return
	}
	if c.WorkerCount <= 0 {
		c.WorkerCount = defaultWorkerCount
	}
	if c.BufferSize <= 0 {
		c.BufferSize = defaultBufferSize
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaultWriteTimeout
	}
}

// IsEnabled reports whether state collection is active.
func (c *Config) IsEnabled() bool {
	return c != nil && c.Enabled
}

// HasWatchResources reports whether at least one rule lists a valid resource.
func (c *Config) HasWatchResources() bool {
	if c == nil {
		return false
	}
	for _, rule := range c.Rules {
		for _, name := range rule.Resources {
			if _, err := k8sresource.Parse(name); err == nil {
				return true
			}
		}
	}
	return false
}

func namespacesFromRules(rules []pkgconfig.StateNamespaceRule) []string {
	if len(rules) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, rule := range rules {
		ns := strings.TrimSpace(rule.Namespace)
		if ns == "" {
			return nil
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}
