package state

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

// demandWatch is one demand-driven informer. Unlike the static-rule path,
// where a factory is shared across every GVR in a rule, each demand-driven
// GVR gets its own factory and cancel func: SharedInformerFactory.Start stops
// every informer registered on it, so a shared factory could never stop just
// one GVR without stopping them all.
type demandWatch struct {
	factory  dynamicinformer.DynamicSharedInformerFactory
	informer cache.SharedIndexInformer
	cancel   context.CancelFunc
}

// reconcile converges the demand-driven informer set on desired: it starts
// whatever is missing, stops whatever is no longer wanted, and leaves
// everything else untouched. Restarting an informer that is staying would
// resync its whole cache, and the pipeline would write that resync as a
// burst of updates indistinguishable from real cluster change.
func (c *Collector) reconcile(ctx context.Context, desired []k8sresource.Descriptor) error {
	if c.dynamic == nil {
		return errors.New("reconcile: dynamic client is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	wanted := make(map[string]k8sresource.Descriptor, len(desired))
	for _, desc := range desired {
		wanted[gvrKey(desc.GVR)] = desc
	}

	c.watchMu.Lock()
	if c.watches == nil {
		c.watches = make(map[string]*demandWatch)
	}

	var toStop []string
	for key := range c.watches {
		if _, ok := wanted[key]; !ok {
			toStop = append(toStop, key)
		}
	}
	for _, key := range toStop {
		w := c.watches[key]
		delete(c.watches, key)
		// Cancel only; do not wait for the informer's goroutines to exit.
		// Stopping does not delete anything from the cluster, so no event
		// may be emitted for what it was holding — canceling the context
		// (rather than draining/faking a delete) is what keeps it silent.
		w.cancel()
	}

	var toStart []k8sresource.Descriptor
	for key, desc := range wanted {
		if _, ok := c.watches[key]; !ok {
			toStart = append(toStart, desc)
		}
	}
	c.watchMu.Unlock()

	if len(toStart) == 0 {
		return nil
	}

	emitter := newEventEmitter(c.workCh, c.metrics)
	for _, desc := range toStart {
		if err := c.startDemandWatch(ctx, emitter, desc); err != nil {
			// A GVR that fails to start (e.g. a CRD deleted between the
			// catalog sweep and the watch request) must not take the rest
			// of the reconcile down with it.
			c.log.Error("start demand-driven informer failed",
				logger.F("gvr", gvrKey(desc.GVR)),
				logger.F("error", err.Error()),
			)
		}
	}
	return nil
}

// startDemandWatch builds and starts one GVR's informer and registers it in
// the watch map. Demand-driven watches are cluster-wide (metav1.NamespaceAll)
// regardless of any namespace scoping static rules use — a viewer's tab is
// not namespace-scoped.
func (c *Collector) startDemandWatch(parentCtx context.Context, emitter eventEmitter, desc k8sresource.Descriptor) error {
	build := c.newDemandInformer
	if build == nil {
		build = c.defaultNewDemandInformer
	}

	factory, informer, err := build(desc)
	if err != nil {
		return err
	}

	wctx, cancel := context.WithCancel(parentCtx)

	if _, err := informer.AddEventHandler(emitter.handlersFor(desc)); err != nil {
		cancel()
		return fmt.Errorf("register event handler for %s: %w", desc.MetricLabel, err)
	}

	factory.Start(wctx.Done())

	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.watches[gvrKey(desc.GVR)] = &demandWatch{
		factory:  factory,
		informer: informer,
		cancel:   cancel,
	}
	return nil
}

// defaultNewDemandInformer is the production factory/informer constructor.
// It is a field on Collector (c.newDemandInformer) rather than a bare
// function call so tests can substitute a failing constructor for one GVR
// without a real cluster or a flaky timing-dependent failure mode.
func (c *Collector) defaultNewDemandInformer(desc k8sresource.Descriptor) (dynamicinformer.DynamicSharedInformerFactory, cache.SharedIndexInformer, error) {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamic,
		c.cfg.ResyncPeriod,
		metav1.NamespaceAll,
		nil,
	)
	informer := factory.ForResource(desc.GVR).Informer()
	return factory, informer, nil
}

// gvrKey renders a GVR as "group/version/resource" (e.g. "batch/v1/cronjobs",
// "/v1/configmaps" for the core group). This is the demand-watch map key; it
// intentionally differs from schema.GroupVersionResource.String()'s
// "group/version, Resource=resource" form, which is meant for log/error text
// rather than as a lookup key.
func gvrKey(gvr schema.GroupVersionResource) string {
	return gvr.Group + "/" + gvr.Version + "/" + gvr.Resource
}
