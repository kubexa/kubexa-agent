package state

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

const (
	eventAdded    = "added"
	eventModified = "modified"
	eventDeleted  = "deleted"
)

// workItem is a Kubernetes watch event queued for worker processing.
type workItem struct {
	desc      k8sresource.Descriptor
	eventType string
	object    runtime.Object
}

type eventEmitter struct {
	workCh  chan<- workItem
	metrics *metrics
}

func newEventEmitter(workCh chan<- workItem, metrics *metrics) eventEmitter {
	return eventEmitter{workCh: workCh, metrics: metrics}
}

func (e eventEmitter) emit(desc k8sresource.Descriptor, eventType string, obj runtime.Object) {
	if obj == nil {
		return
	}
	item := workItem{
		desc:      desc,
		eventType: eventType,
		object:    obj,
	}
	select {
	case e.workCh <- item:
	default:
		if e.metrics != nil {
			e.metrics.incDropped(desc.MetricLabel, "work_queue_full")
		}
	}
}

func (e eventEmitter) handlersFor(desc k8sresource.Descriptor) cache.ResourceEventHandlerFuncs {
	if desc.SkipAdded {
		return cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, obj any) { e.onUpdate(desc, obj) },
			DeleteFunc: func(obj any) { e.onDelete(desc, obj) },
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { e.onAdd(desc, obj) },
		UpdateFunc: func(_, obj any) { e.onUpdate(desc, obj) },
		DeleteFunc: func(obj any) { e.onDelete(desc, obj) },
	}
}

func (e eventEmitter) onAdd(desc k8sresource.Descriptor, obj any) {
	runtimeObj, ok := objectFromInformer(obj)
	if !ok {
		if e.metrics != nil {
			e.metrics.incDropped(desc.MetricLabel, "invalid_object")
		}
		return
	}
	e.emit(desc, eventAdded, runtimeObj)
}

func (e eventEmitter) onUpdate(desc k8sresource.Descriptor, obj any) {
	runtimeObj, ok := objectFromInformer(obj)
	if !ok {
		if e.metrics != nil {
			e.metrics.incDropped(desc.MetricLabel, "invalid_object")
		}
		return
	}
	e.emit(desc, eventModified, runtimeObj)
}

func (e eventEmitter) onDelete(desc k8sresource.Descriptor, obj any) {
	runtimeObj, ok := objectFromInformer(obj)
	if !ok {
		if e.metrics != nil {
			e.metrics.incDropped(desc.MetricLabel, "invalid_object")
		}
		return
	}
	e.emit(desc, eventDeleted, runtimeObj)
}

func objectFromInformer(obj any) (runtime.Object, bool) {
	if obj == nil {
		return nil, false
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	runtimeObj, ok := obj.(runtime.Object)
	if !ok {
		return nil, false
	}
	return runtimeObj.DeepCopyObject(), true
}
