package metrics

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

// defaultKubeletPort is used when a node's status reports no kubelet endpoint
// port. 10250 is the read-write authenticated port; 10255 is the deprecated
// read-only one and is off by default on every supported version.
const defaultKubeletPort = "10250"

// defaultNodeRefreshInterval bounds how long a new node waits before it is
// scraped, and how long a removed node's scraper keeps failing. A node join is
// a minutes-scale event and a LIST of nodes is one API call, so this is not on
// any hot path.
const defaultNodeRefreshInterval = 2 * time.Minute

// TargetTemplate describes a family of scrape targets that are generated from
// cluster state rather than written out in configuration.
//
// Kind names the family and is what health reporting aggregates on: "cadvisor"
// is one row on the ingestion screen no matter how many nodes back it, because
// an operator reading "3 of 40 cAdvisor targets failing" learns more than they
// would from forty rows.
type TargetTemplate struct {
	Kind       string
	NamePrefix string
	Scheme     string
	// Port overrides the node's own reported kubelet port. Empty means "use
	// the node's port, and defaultKubeletPort when the node reports none".
	Port string
	Path string

	Interval        time.Duration
	Timeout         time.Duration
	Labels          map[string]string
	BearerTokenPath string
	TLSConfig       TLSConfig
	MetricAllowlist []string
	MetricDenylist  []string
}

// DynamicTargetsConfig configures generated scrape targets.
type DynamicTargetsConfig struct {
	Templates       []TargetTemplate
	RefreshInterval time.Duration
}

// targetsForNodes expands one template over a node inventory.
func targetsForNodes(tpl TargetTemplate, nodes []k8s.NodeInfo) []ScrapeTarget {
	out := make([]ScrapeTarget, 0, len(nodes))
	for _, node := range nodes {
		if node.InternalIP == "" {
			continue
		}
		port := tpl.Port
		if port == "" {
			port = defaultKubeletPort
			if node.KubeletPort > 0 {
				port = strconv.Itoa(int(node.KubeletPort))
			}
		}
		scheme := tpl.Scheme
		if scheme == "" {
			scheme = "https"
		}

		// A fresh map per target. Sharing the template's map would give every
		// target the last node's name.
		labels := make(map[string]string, len(tpl.Labels)+1)
		for k, v := range tpl.Labels {
			labels[k] = v
		}
		labels["node"] = node.Name

		interval := tpl.Interval
		if interval <= 0 {
			interval = pkgconfig.DefaultScrapeInterval
		}
		timeout := tpl.Timeout
		if timeout <= 0 {
			timeout = pkgconfig.DefaultScrapeTimeout
		}

		out = append(out, ScrapeTarget{
			Name:            fmt.Sprintf("%s/%s", tpl.NamePrefix, node.Name),
			URL:             scheme + "://" + net.JoinHostPort(node.InternalIP, port) + tpl.Path,
			Interval:        interval,
			Timeout:         timeout,
			Labels:          labels,
			BearerTokenPath: tpl.BearerTokenPath,
			TLSConfig:       tpl.TLSConfig,
			MetricAllowlist: append([]string(nil), tpl.MetricAllowlist...),
			MetricDenylist:  append([]string(nil), tpl.MetricDenylist...),
		})
	}
	return out
}

// targetIdentity is what makes two generated targets the same target. The URL
// is part of it: a node that keeps its name and changes its address is a new
// target, and leaving the old scraper running would attribute its failures to
// a live node.
func targetIdentity(t ScrapeTarget) string { return t.Name + "\x00" + t.URL }

// diffTargets reports which targets are new in next and which are gone from
// prev.
func diffTargets(prev, next []ScrapeTarget) (added, removed []ScrapeTarget) {
	prevByID := make(map[string]ScrapeTarget, len(prev))
	for _, t := range prev {
		prevByID[targetIdentity(t)] = t
	}
	nextByID := make(map[string]ScrapeTarget, len(next))
	for _, t := range next {
		nextByID[targetIdentity(t)] = t
	}

	for _, t := range next {
		if _, ok := prevByID[targetIdentity(t)]; !ok {
			added = append(added, t)
		}
	}
	for _, t := range prev {
		if _, ok := nextByID[targetIdentity(t)]; !ok {
			removed = append(removed, t)
		}
	}
	return added, removed
}

// nodeLister is the slice of k8s.Client this file needs. Narrowing it keeps
// the provider's tests free of a whole fake cluster client.
type nodeLister interface {
	Nodes(ctx context.Context) ([]k8s.NodeInfo, error)
}

// dynamicProvider re-expands its templates over the node inventory on an
// interval and reports the delta.
type dynamicProvider struct {
	lister    nodeLister
	templates []TargetTemplate
	interval  time.Duration
	log       *logger.Logger

	current []ScrapeTarget
}

func newDynamicProvider(lister nodeLister, cfg DynamicTargetsConfig, log *logger.Logger) *dynamicProvider {
	interval := cfg.RefreshInterval
	if interval <= 0 {
		interval = defaultNodeRefreshInterval
	}
	return &dynamicProvider{
		lister:    lister,
		templates: cfg.Templates,
		interval:  interval,
		log:       log,
	}
}

// refresh lists nodes once and returns the targets to start and to stop.
//
// On a listing error it returns no delta at all rather than an empty target
// set. Treating a failed LIST as "there are no nodes" would tear down every
// scraper on one API blip, and the graphs would go to zero for a reason that
// has nothing to do with the nodes.
func (p *dynamicProvider) refresh(ctx context.Context) (added, removed []ScrapeTarget, err error) {
	nodes, err := p.lister.Nodes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list nodes for dynamic targets: %w", err)
	}

	var next []ScrapeTarget
	for _, tpl := range p.templates {
		next = append(next, targetsForNodes(tpl, nodes)...)
	}

	added, removed = diffTargets(p.current, next)
	p.current = next
	return added, removed, nil
}
