package logs

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/kubexa/kubexa-agent/internal/logger"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

func (c *Collector) ruleViewFor(rule pkgconfig.LogNamespaceRule) ruleView {
	return ruleView{
		id:         rule.ID,
		ruleIDs:    []string{rule.ID},
		tailLines:  rule.ResolveTailLines(c.cfg.TailLines),
		follow:     rule.ResolveFollow(c.cfg.Follow),
		labelSel:   rule.EffectiveLabelSelector(),
		fieldSel:   rule.FieldSelector,
		podNames:   append([]string(nil), rule.PodNames...),
		containers: append([]string(nil), rule.Containers...),
	}
}

func mergeRuleView(a, b ruleView) ruleView {
	out := a
	if b.tailLines > out.tailLines {
		out.tailLines = b.tailLines
	}
	out.follow = out.follow || b.follow
	out.containers = mergeContainerNames(out.containers, b.containers)
	out.ruleIDs = appendRuleIDs(out.ruleIDs, b.ruleIDs...)
	return out
}

func appendRuleIDs(existing []string, ids ...string) []string {
	if len(ids) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(ids))
	out := make([]string, 0, len(existing)+len(ids))
	for _, id := range append(existing, ids...) {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (c *Collector) addTarget(desired map[streamKey]streamTarget, target streamTarget) {
	if existing, ok := desired[target.key]; ok {
		if len(existing.rule.ruleIDs) > 0 || target.rule.id != "" {
			mergedIDs := appendRuleIDs(existing.rule.ruleIDs, target.rule.ruleIDs...)
			if len(mergedIDs) > 1 {
				c.log.Debug("merged log collection rules for pod container",
					logger.F("namespace", target.pod.Namespace),
					logger.F("pod", target.pod.Name),
					logger.F("container", target.container),
					logger.F("rules", mergedIDs),
				)
			}
		}
		target.rule = mergeRuleView(existing.rule, target.rule)
	}
	desired[target.key] = target
}

func (c *Collector) targetsForPod(pod *corev1.Pod) map[streamKey]streamTarget {
	desired := make(map[streamKey]streamTarget)
	if !podEligible(pod, c.cfg.ExcludeNamespaces) {
		return desired
	}

	for _, rule := range c.cfg.Rules {
		view := c.ruleViewFor(rule)
		if !podMatchesRule(pod, rule.Namespace, view) {
			continue
		}
		if !matchesPodName(pod.Name, view.podNames) {
			continue
		}
		for _, container := range containersForPod(pod, view.containers) {
			key := streamKey{podUID: string(pod.UID), containerName: container}
			c.addTarget(desired, streamTarget{
				key:       key,
				pod:       pod,
				container: container,
				rule:      view,
			})
		}
	}
	return desired
}

func podMatchesRule(pod *corev1.Pod, ruleNamespace string, view ruleView) bool {
	if pod == nil {
		return false
	}
	if ruleNamespace != "" && pod.Namespace != ruleNamespace {
		return false
	}
	// Label and field selectors are enforced by list/watch API calls; no local re-check here.
	_ = view
	return true
}

func (c *Collector) buildDesired(ctx context.Context) map[streamKey]streamTarget {
	desired := make(map[streamKey]streamTarget)

	for _, rule := range c.cfg.Rules {
		view := c.ruleViewFor(rule)
		pods, err := c.listPods(ctx, rule.Namespace, view)
		if err != nil {
			c.log.Warn("pod list failed",
				logger.F("rule_id", rule.ID),
				logger.F("namespace", rule.Namespace),
				logger.F("error", err.Error()),
			)
			continue
		}
		for i := range pods {
			pod := &pods[i]
			if !podEligible(pod, c.cfg.ExcludeNamespaces) {
				continue
			}
			if !matchesPodName(pod.Name, view.podNames) {
				continue
			}
			for _, container := range containersForPod(pod, view.containers) {
				key := streamKey{podUID: string(pod.UID), containerName: container}
				c.addTarget(desired, streamTarget{
					key:       key,
					pod:       pod,
					container: container,
					rule:      view,
				})
			}
		}
	}
	return desired
}

func (c *Collector) resync(ctx context.Context) {
	desired := c.buildDesired(ctx)
	c.reconcileStreams(ctx, desired, true)
}

func (c *Collector) handlePodEvent(ctx context.Context, pod *corev1.Pod, eventType watch.EventType) {
	if pod == nil {
		return
	}
	switch eventType {
	case watch.Deleted:
		c.removePodStreams(string(pod.UID))
		return
	case watch.Added, watch.Modified:
	default:
		return
	}

	desired := c.targetsForPod(pod)
	if len(desired) == 0 {
		c.removePodStreams(string(pod.UID))
		return
	}
	c.reconcileStreams(ctx, desired, false)
}

func (c *Collector) removePodStreams(podUID string) {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	for key, handle := range c.streams {
		if key.podUID == podUID {
			handle.cancel()
			delete(c.streams, key)
		}
	}
	c.updateActiveStreamsMetricLocked()
}

func (c *Collector) watchRule(ctx context.Context, rule pkgconfig.LogNamespaceRule) {
	view := c.ruleViewFor(rule)
	ns := rule.Namespace
	if ns == "" {
		ns = metav1.NamespaceAll
	}

	attempt := 0
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec

	for {
		if err := ctx.Err(); err != nil {
			return
		}

		watcher, err := c.kube.Pods(ns).Watch(ctx, metav1.ListOptions{
			LabelSelector: view.labelSel,
			FieldSelector: view.fieldSel,
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				c.log.Warn("pod watch failed",
					logger.F("rule_id", rule.ID),
					logger.F("namespace", rule.Namespace),
					logger.F("error", err.Error()),
				)
			}
			delay := streamBackoff(attempt, rng)
			attempt++
			if err := c.sleep(ctx, delay); err != nil {
				return
			}
			continue
		}

		attempt = 0
		c.consumePodWatch(ctx, watcher, rule.ID)
		watcher.Stop()

		if err := ctx.Err(); err != nil {
			return
		}
		c.log.Info("pod watch closed; reconnecting",
			logger.F("rule_id", rule.ID),
			logger.F("namespace", rule.Namespace),
		)
		delay := streamBackoff(attempt, rng)
		attempt++
		if err := c.sleep(ctx, delay); err != nil {
			return
		}
	}
}

func (c *Collector) consumePodWatch(ctx context.Context, watcher watch.Interface, ruleID string) {
	ch := watcher.ResultChan()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			pod, err := podFromWatchEvent(event)
			if err != nil {
				c.log.Warn("pod watch event skipped",
					logger.F("rule_id", ruleID),
					logger.F("type", string(event.Type)),
					logger.F("error", err.Error()),
				)
				continue
			}
			c.handlePodEvent(ctx, pod, event.Type)
		}
	}
}

func podFromWatchEvent(event watch.Event) (*corev1.Pod, error) {
	if event.Object == nil {
		return nil, errors.New("watch event object is nil")
	}
	raw := any(event.Object)
	if tombstone, ok := raw.(cache.DeletedFinalStateUnknown); ok {
		raw = tombstone.Obj
	}
	pod, ok := raw.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("unexpected watch object %T", event.Object)
	}
	return pod, nil
}
