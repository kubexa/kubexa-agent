package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"

	"github.com/kubexa/kubexa-agent/internal/config"
)

// ─────────────────────────────────────────
// Types
// ─────────────────────────────────────────

// EventType kubernetes resource event type
type EventType string

const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
)

// PodEvent event from watched pod
type PodEvent struct {
	Type      EventType
	Pod       *corev1.Pod
	OldPod    *corev1.Pod // set for MODIFIED when the informer provides the previous object
	Timestamp time.Time
}

// ─────────────────────────────────────────
// PodWatcher
// ─────────────────────────────────────────

// PodWatcher watches multiple namespaces with SharedInformer.
// Creates a separate informer for each namespace.
type PodWatcher struct {
	client    *Client
	cfg       config.WatchConfig
	eventChan chan *PodEvent
	stopChan  chan struct{}
	informers []cache.Controller
}

func NewPodWatcher(
	client *Client,
	cfg config.WatchConfig,
	bufferSize int,
) *PodWatcher {
	return &PodWatcher{
		client:    client,
		cfg:       cfg,
		eventChan: make(chan *PodEvent, bufferSize),
		stopChan:  make(chan struct{}),
	}
}

// ─────────────────────────────────────────
// Start
// ─────────────────────────────────────────

// Start starts informers for all namespaces.
// context cancelled, all informers stop.
func (w *PodWatcher) Start(ctx context.Context) error {
	if len(w.cfg.Namespaces) == 0 {
		return fmt.Errorf("at least one namespace must be defined")
	}

	for _, ns := range w.cfg.Namespaces {
		informer, err := w.startNamespaceInformer(ctx, ns)
		if err != nil {
			return fmt.Errorf(
				"namespace informer failed to start (%s): %w",
				ns.Namespace, err,
			)
		}
		w.informers = append(w.informers, informer)

		log.Info().
			Str("namespace", ns.Namespace).
			Str("label_selector", ns.LabelSelector).
			Msg("pod watcher started")
	}

	// context cancelled, stop
	go func() {
		<-ctx.Done()
		w.Stop()
	}()

	return nil
}

// ─────────────────────────────────────────
// Namespace Informer
// ─────────────────────────────────────────

func (w *PodWatcher) startNamespaceInformer(
	ctx context.Context,
	ns config.NamespaceSelector,
) (cache.Controller, error) {
	// field selector with label filter
	selector := fields.Everything()
	if ns.LabelSelector != "" {
		// label selector uses ListWatch options
		// fields.Everything() is enough here,
		// label filter handler in
		_ = ns.LabelSelector
	}

	listWatcher := cache.NewListWatchFromClient(
		w.client.Clientset.CoreV1().RESTClient(),
		"pods",
		ns.Namespace,
		selector,
	)

	// event handler
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			// label selector filter
			if !w.matchesLabelSelector(pod, ns.LabelSelector) {
				return
			}
			w.sendEvent(EventAdded, pod, nil)
		},
		UpdateFunc: func(oldObj, newObj any) {
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				return
			}
			if !w.matchesLabelSelector(newPod, ns.LabelSelector) {
				return
			}
			var oldPod *corev1.Pod
			if op, ok := oldObj.(*corev1.Pod); ok {
				oldPod = op
			}
			w.sendEvent(EventModified, newPod, oldPod)
		},
		DeleteFunc: func(obj any) {
			// handle DeletedFinalStateUnknown state
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				// tombstone check
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					log.Warn().Msg("delete event: unknown object type")
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			if !w.matchesLabelSelector(pod, ns.LabelSelector) {
				return
			}
			w.sendEvent(EventDeleted, pod, nil)
		},
	}

	_, controller := cache.NewInformerWithOptions(
		cache.InformerOptions{
			ListerWatcher: listWatcher,
			ObjectType:    &corev1.Pod{},
			ResyncPeriod:  w.cfg.ResyncInterval,
			Handler:       handler,
		},
	)
	// start informer
	go controller.Run(w.stopChan)

	// wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), controller.HasSynced) {
		return nil, fmt.Errorf(
			"cache sync timeout (namespace: %s)",
			ns.Namespace,
		)
	}

	log.Debug().
		Str("namespace", ns.Namespace).
		Msg("cache sync completed")

	return controller, nil
}

// ─────────────────────────────────────────
// Event Channel
// ─────────────────────────────────────────

// Events event channel.
// Caller reads from this channel to get pod events.
func (w *PodWatcher) Events() <-chan *PodEvent {
	return w.eventChan
}

func (w *PodWatcher) sendEvent(eventType EventType, pod *corev1.Pod, oldPod *corev1.Pod) {
	event := &PodEvent{
		Type:      eventType,
		Pod:       pod,
		OldPod:    oldPod,
		Timestamp: time.Now(),
	}

	select {
	case w.eventChan <- event:
		log.Debug().
			Str("event", string(eventType)).
			Str("pod", pod.Name).
			Str("namespace", pod.Namespace).
			Str("phase", string(pod.Status.Phase)).
			Msg("pod event sent")
	default:
		// channel full, event dropped
		log.Warn().
			Str("event", string(eventType)).
			Str("pod", pod.Name).
			Str("namespace", pod.Namespace).
			Msg("event channel full, pod event dropped")
	}
}

// ─────────────────────────────────────────
// Label Selector
// ─────────────────────────────────────────

// matchesLabelSelector checks if the pod matches the label selector.
// if selector is empty, every pod matches.
func (w *PodWatcher) matchesLabelSelector(pod *corev1.Pod, selector string) bool {
	if selector == "" {
		return true
	}

	// "key=value,key2=value2" format to parse
	requirements := parseSelector(selector)
	for key, value := range requirements {
		if podValue, ok := pod.Labels[key]; !ok || podValue != value {
			return false
		}
	}
	return true
}

func parseSelector(selector string) map[string]string {
	result := make(map[string]string)
	if selector == "" {
		return result
	}

	// "app=myapp,tier=backend" → map[app:myapp tier:backend] (key=value pairs)
	pairs := splitSelector(selector)
	for _, pair := range pairs {
		for i := 0; i < len(pair); i++ {
			if pair[i] == '=' {
				key := pair[:i]
				value := pair[i+1:]
				result[key] = value
				break
			}
		}
	}
	return result
}

func splitSelector(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// ─────────────────────────────────────────
// Stop
// ─────────────────────────────────────────

func (w *PodWatcher) Stop() {
	select {
	case <-w.stopChan:
		// already closed
	default:
		close(w.stopChan)
		log.Info().Msg("pod watcher stopped")
	}
}
