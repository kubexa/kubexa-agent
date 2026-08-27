package metrics

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

// probeKubeStateService reports whether the kube-state-metrics Service exists.
//
// The answer is used to choose between "not installed" and "unreachable", and
// those send an operator to two different places -- so a failure to ASK must
// never be reported as an answer. A NotFound is absence; every other error is
// an error.
func probeKubeStateService(ctx context.Context, kube kubernetes.Interface, namespace, name string) (bool, error) {
	_, err := kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("probe kube-state-metrics service %s/%s: %w", namespace, name, err)
	}
}

// runKubeStateTarget alternates between probing for the Service and scraping
// it. While the Service is absent the target reports StateNotInstalled and no
// scrape is attempted at all: a connection error every minute against
// something that was never installed is noise, and it reads on the screen as
// a broken integration rather than an absent one.
func (c *Collector) runKubeStateTarget(ctx context.Context) {
	ks := c.cfg.KubeState
	filter, err := NewMetricFilter(ks.Target.MetricAllowlist, ks.Target.MetricDenylist)
	if err != nil {
		c.log.Warn("kube-state-metrics filter rejected", logger.F("error", err.Error()))
		return
	}

	const kind = "kube_state_metrics"
	targetName := targetLabel(ks.Target)
	installed := !ks.ProbeService
	if installed {
		// An explicit URL was configured: there is no Service to probe, so
		// the target is assumed present and reported as one target from the
		// start -- nothing else ever calls SetTargetCount for this kind.
		c.health.SetTargetCount(kind, 1)
	}
	lastProbe := time.Time{}

	c.waitStartupJitter(ctx, ks.Target.Interval)
	ticker := time.NewTicker(ks.Target.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if ks.ProbeService && time.Since(lastProbe) >= ks.ProbeInterval {
			found, probeErr := probeKubeStateService(ctx, c.kube.Clientset(), ks.ServiceNamespace, ks.ServiceName)
			lastProbe = time.Now()
			switch {
			case probeErr != nil:
				// Could not ask. Leave the previous verdict standing rather
				// than claiming absence on an API blip.
				c.log.Warn("kube-state-metrics presence probe failed",
					logger.F("error", probeErr.Error()))
			case !found:
				installed = false
				c.health.MarkNotInstalled(kind)
			default:
				installed = true
				c.health.SetTargetCount(kind, 1)
			}
		}

		if installed {
			result, err := c.custom.ScrapeTarget(ctx, ks.Target, filter)
			if err != nil {
				c.health.RecordFailure(kind, targetName, err)
				c.log.Warn("kube-state-metrics scrape failed",
					logger.F("url", ks.Target.URL),
					logger.F("error", err.Error()))
			} else {
				c.health.RecordSuccess(kind, targetName, countSamples(result.Families))
				if err := c.publishPrometheusMetrics(ctx, ks.Target, result); err != nil {
					c.log.Warn("publish kube-state-metrics failed", logger.F("error", err.Error()))
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
