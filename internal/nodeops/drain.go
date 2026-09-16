// internal/nodeops/drain.go
package nodeops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func (e *Engine) runDrain(ctx context.Context, id, node string, d *agentv1.DrainOptions, emit Emitter) *agentv1.NodeJobEvent {
	cs := e.opts.Clientset
	s := &snapshot{id: id}
	dryRun := d.GetDryRun()

	list, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		if ctx.Err() != nil {
			return endEarly(s, ctx.Err())
		}
		code, msg := codeFor(err, "pods list")
		return s.refused(code, msg)
	}
	pre := Classify(list.Items, e.opts.Own, d.GetForce(), d.GetDeleteEmptydirData())
	s.pods = pre.Pods
	if len(pre.NeedsForce) > 0 {
		return s.refused(agentv1.NodeJobErrorCode_NODE_JOB_ERROR_NEEDS_FORCE,
			fmt.Sprintf("%d pod(s) have no controller and would not be recreated; set force to evict them: %s",
				len(pre.NeedsForce), strings.Join(pre.NeedsForce, ", ")))
	}
	if len(pre.NeedsEmptyDir) > 0 {
		return s.refused(agentv1.NodeJobErrorCode_NODE_JOB_ERROR_NEEDS_EMPTYDIR,
			fmt.Sprintf("%d pod(s) mount emptyDir volumes whose data would be lost; set delete_emptydir_data to evict them: %s",
				len(pre.NeedsEmptyDir), strings.Join(pre.NeedsEmptyDir, ", ")))
	}
	emit(s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_ACCEPTED, fmt.Sprintf("%d pods on the node", len(s.pods))))

	if _, err := SetUnschedulable(ctx, cs, node, true, dryRun); err != nil {
		if ctx.Err() != nil {
			return endEarly(s, ctx.Err())
		}
		code, msg := codeFor(err, "nodes patch")
		return s.failed(code, msg)
	}
	s.nodeCordoned = !dryRun
	emit(s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_RUNNING, "node cordoned; evicting"))

	var grace *int64
	if g := d.GetGracePeriodSeconds(); g >= 0 {
		grace = &g
	}
	nextRetry := map[string]time.Time{}
	for {
		now := time.Now()
		for i := range s.pods {
			if ctx.Err() != nil {
				break
			}
			p := &s.pods[i]
			key := p.Namespace + "/" + p.Name
			switch p.State {
			case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED:
				// A dry run's BLOCKED is the answer, not a wait (see the
				// "remaining" count below) -- it must never be re-attempted,
				// regardless of nextRetry timing.
				if p.State == agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED && (dryRun || now.Before(nextRetry[key])) {
					continue
				}
				err := evict(ctx, cs, p.Namespace, p.Name, grace, dryRun)
				switch {
				case err == nil:
					p.State, p.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING, ""
				case ctx.Err() != nil:
					// Cancel or the deadline fired during this call. Leave
					// the pod's state untouched -- the ctx.Err() check
					// below ends the job CANCELLED/TIMEOUT; marking a
					// still-present pod FAILED here would be wrong.
				case isPDBBlocked(err):
					p.State, p.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED, ReasonPDB
					nextRetry[key] = now.Add(e.opts.RetryInterval)
				case apierrors.IsNotFound(err):
					p.State, p.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_GONE, ""
				case apierrors.IsForbidden(err):
					code, msg := codeFor(err, "pods/eviction create")
					return s.failed(code, msg)
				default:
					p.State, p.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_FAILED, err.Error()
				}
			case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING:
				if dryRun {
					p.State, p.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_GONE, ReasonDryRun
					continue
				}
				cur, err := cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
				if apierrors.IsNotFound(err) || (err == nil && cur.UID != p.UID) {
					p.State, p.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_GONE, ""
				}
				// any other error: still evicting; look again next tick
			}
		}
		if err := ctx.Err(); err != nil {
			return endEarly(s, err)
		}
		remaining := 0
		anyFailed := false
		for _, p := range s.pods {
			switch p.State {
			case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING:
				remaining++
			case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED:
				if !dryRun { // a dry run's BLOCKED is the answer, not a wait
					remaining++
				}
			case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_FAILED:
				anyFailed = true
			}
		}
		if remaining == 0 {
			if anyFailed {
				return s.failed(agentv1.NodeJobErrorCode_NODE_JOB_ERROR_EVICTION_FAILED, "at least one pod could not be evicted")
			}
			msg := "node drained"
			if dryRun {
				msg = "dry run: nothing was changed"
			}
			return s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED, msg)
		}
		emit(s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_RUNNING, fmt.Sprintf("waiting for %d pods", remaining)))
		select {
		case <-ctx.Done():
			return endEarly(s, ctx.Err())
		case <-time.After(e.opts.PollInterval):
		}
	}
}

// endEarly builds the terminal event for a job the context ended: Cancel
// is the only source of context.Canceled (Start's own cancel runs after
// run returns), and the deadline the only source of DeadlineExceeded.
func endEarly(s *snapshot, err error) *agentv1.NodeJobEvent {
	if errors.Is(err, context.Canceled) {
		msg := "cancelled"
		if s.nodeCordoned {
			msg = "cancelled; the node is still cordoned"
		}
		return s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_CANCELLED, msg)
	}
	var left []string
	for _, p := range s.pods {
		switch p.State {
		case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING, agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED:
			left = append(left, p.Namespace+"/"+p.Name)
		}
	}
	return s.failed(agentv1.NodeJobErrorCode_NODE_JOB_ERROR_TIMEOUT,
		fmt.Sprintf("timed out with %d pod(s) still on the node: %s", len(left), strings.Join(left, ", ")))
}

func evict(ctx context.Context, cs kubernetes.Interface, ns, name string, grace *int64, dryRun bool) error {
	ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if grace != nil || dryRun {
		ev.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: grace}
		if dryRun {
			ev.DeleteOptions.DryRun = []string{metav1.DryRunAll}
		}
	}
	return cs.PolicyV1().Evictions(ns).Evict(ctx, ev)
}

// isPDBBlocked: the Eviction API answers 429 when a PodDisruptionBudget
// forbids the disruption right now; older servers wrapped the same text in
// a 500. Both mean "retry later", never "give up".
func isPDBBlocked(err error) bool {
	if apierrors.IsTooManyRequests(err) {
		return true
	}
	return apierrors.IsInternalError(err) && strings.Contains(err.Error(), "disruption budget")
}
