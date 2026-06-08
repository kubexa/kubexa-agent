package metrics

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const (
	k8sSourceLabel    = "kubernetes"
	k8sPodTarget      = "pods"
	k8sNodeTarget     = "nodes"
	metricsAPIBackoff = 30 * time.Second
)

// k8sMetricsScraper scrapes metrics.k8s.io for pod and node resource usage.
type k8sMetricsScraper struct {
	kube    k8s.Client
	metrics *scraperMetrics
	log     *logger.Logger

	metricsAPIWarnOnce sync.Once
}

func newK8sMetricsScraper(kube k8s.Client, metrics *scraperMetrics) *k8sMetricsScraper {
	return &k8sMetricsScraper{
		kube:    kube,
		metrics: metrics,
	}
}

func (s *k8sMetricsScraper) setLogger(log *logger.Logger) {
	s.log = log
}

// ScrapePodsForRule collects pod metrics matching the rule filters.
func (s *k8sMetricsScraper) ScrapePodsForRule(ctx context.Context, rule KubeMetricsRule) ([]*agentv1.MetricsEvent, error) {
	start := time.Now()
	target := scrapeTargetLabel(k8sPodTarget, rule)

	allowed, err := s.allowedPods(ctx, rule)
	if err != nil {
		s.finishScrape(k8sSourceLabel, target, start, 0, err)
		return nil, err
	}

	podMetrics, err := s.kube.PodMetrics(ctx, rule.Namespace)
	if err != nil {
		s.finishScrape(k8sSourceLabel, target, start, 0, err)
		if isMetricsAPIUnavailable(err) {
			return nil, err
		}
		return nil, fmt.Errorf("rule %q: list pod metrics: %w", rule.ID, err)
	}

	events := make([]*agentv1.MetricsEvent, 0, len(podMetrics))
	for _, m := range podMetrics {
		if !matchesName(m.Name, rule.PodNames) {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[podKey(m.Namespace, m.Name)]; !ok {
				continue
			}
		}
		events = append(events, podMetricEvent(m))
	}

	s.finishScrape(k8sSourceLabel, target, start, len(events), nil)
	return events, nil
}

// ScrapeNodesForRule collects node metrics matching the rule filters.
func (s *k8sMetricsScraper) ScrapeNodesForRule(ctx context.Context, rule KubeMetricsRule) ([]*agentv1.MetricsEvent, error) {
	start := time.Now()
	target := scrapeTargetLabel(k8sNodeTarget, rule)

	nodeMetrics, err := s.kube.NodeMetrics(ctx)
	if err != nil {
		s.finishScrape(k8sSourceLabel, target, start, 0, err)
		return nil, err
	}

	events := make([]*agentv1.MetricsEvent, 0, len(nodeMetrics))
	for _, m := range nodeMetrics {
		if !matchesName(m.Name, rule.NodeNames) {
			continue
		}
		events = append(events, nodeMetricEvent(m))
	}

	s.finishScrape(k8sSourceLabel, target, start, len(events), nil)
	return events, nil
}

func (s *k8sMetricsScraper) allowedPods(ctx context.Context, rule KubeMetricsRule) (map[string]struct{}, error) {
	if !ruleNeedsPodAPIFilter(rule) {
		return nil, nil
	}

	ns := rule.Namespace
	if ns == "" {
		ns = metav1.NamespaceAll
	}

	list, err := s.kube.Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: rule.LabelSelector,
		FieldSelector: rule.FieldSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("rule %q: list pods: %w", rule.ID, err)
	}

	allowed := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if !matchesName(pod.Name, rule.PodNames) {
			continue
		}
		allowed[podKey(pod.Namespace, pod.Name)] = struct{}{}
	}
	return allowed, nil
}

func (s *k8sMetricsScraper) finishScrape(source, target string, start time.Time, collected int, err error) {
	s.metrics.observeScrape(source, target, time.Since(start))
	if err != nil {
		s.metrics.incScrape(source, target, "error")
		if isMetricsAPIUnavailable(err) {
			s.warnMetricsAPIOnce(err)
		}
		return
	}
	s.metrics.incScrape(source, target, "success")
	s.metrics.setLastScrape(source, target, time.Now().UTC())
	s.metrics.addCollected(source, collected)
}

func (s *k8sMetricsScraper) warnMetricsAPIOnce(err error) {
	s.metricsAPIWarnOnce.Do(func() {
		if s.log != nil {
			s.log.Warn("kubernetes metrics API unavailable; will retry with backoff",
				logger.F("error", err.Error()),
			)
		}
	})
}

func podMetricEvent(m k8s.PodMetric) *agentv1.MetricsEvent {
	return &agentv1.MetricsEvent{
		ResourceType:  agentv1.KubeMetricsResourceType_KUBE_METRICS_RESOURCE_TYPE_POD,
		Namespace:     m.Namespace,
		Name:          m.Name,
		CpuMillicores: m.CPUMillicores,
		MemoryBytes:   m.MemoryBytes,
		Timestamp:     m.Timestamp.UTC().UnixMilli(),
		WindowSeconds: windowSeconds(m.Window),
	}
}

func nodeMetricEvent(m k8s.NodeMetric) *agentv1.MetricsEvent {
	return &agentv1.MetricsEvent{
		ResourceType:  agentv1.KubeMetricsResourceType_KUBE_METRICS_RESOURCE_TYPE_NODE,
		Name:          m.Name,
		CpuMillicores: m.CPUMillicores,
		MemoryBytes:   m.MemoryBytes,
		Timestamp:     m.Timestamp.UTC().UnixMilli(),
		WindowSeconds: windowSeconds(m.Window),
	}
}

func windowSeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	sec := int32(d / time.Second)
	if sec <= 0 {
		return 1
	}
	return sec
}

func scrapeTargetLabel(resource string, rule KubeMetricsRule) string {
	if rule.ID != "" {
		return resource + "/" + rule.ID
	}
	return resource
}

func isMetricsAPIUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "metrics.k8s.io") ||
		strings.Contains(msg, "the server could not find the requested resource")
}

// MetricsAPIBackoff returns the delay before retrying when the metrics API is unavailable.
func MetricsAPIBackoff() time.Duration {
	return metricsAPIBackoff
}

// ScrapePodsForRuleWithBackoff wraps ScrapePodsForRule with recommended backoff on API errors.
func (s *k8sMetricsScraper) ScrapePodsForRuleWithBackoff(ctx context.Context, rule KubeMetricsRule) ([]*agentv1.MetricsEvent, time.Duration, error) {
	events, err := s.ScrapePodsForRule(ctx, rule)
	if err == nil {
		return events, 0, nil
	}
	if isMetricsAPIUnavailable(err) {
		return nil, MetricsAPIBackoff(), err
	}
	return events, initialBackoff, err
}

// ScrapeNodesForRuleWithBackoff wraps ScrapeNodesForRule with recommended backoff on API errors.
func (s *k8sMetricsScraper) ScrapeNodesForRuleWithBackoff(ctx context.Context, rule KubeMetricsRule) ([]*agentv1.MetricsEvent, time.Duration, error) {
	events, err := s.ScrapeNodesForRule(ctx, rule)
	if err == nil {
		return events, 0, nil
	}
	if isMetricsAPIUnavailable(err) {
		return nil, MetricsAPIBackoff(), err
	}
	return events, initialBackoff, fmt.Errorf("rule %q: scrape node metrics: %w", rule.ID, err)
}
