// Package state watches Kubernetes API objects via shared informers and enqueues state events.
package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"google.golang.org/protobuf/proto"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/queue"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const componentName = "state-watcher"

// Writer buffers AgentMessage values for downstream export.
type Writer interface {
	Write(ctx context.Context, msg *agentv1.AgentMessage) error
}

type queueWriter struct {
	q       queue.Queue
	timeout time.Duration
}

func (w *queueWriter) Write(ctx context.Context, msg *agentv1.AgentMessage) error {
	if msg == nil {
		return errors.New("agent message is nil")
	}
	if msg.MessageId == "" {
		msg.MessageId = uuid.NewString()
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal agent message: %w", err)
	}
	item := queue.Item{
		ID:         msg.MessageId,
		Payload:    payload,
		EnqueuedAt: time.Now().UTC(),
	}
	writeCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.q.Enqueue(writeCtx, item); err != nil {
		return fmt.Errorf("enqueue state message: %w", err)
	}
	return nil
}

// Collector watches Kubernetes resources and publishes state change events.
type Collector struct {
	cfg       Config
	dynamic   dynamic.Interface
	writer    Writer
	log       *logger.Logger
	metrics   *metrics
	agentMeta *commonv1.AgentMetadata

	workCh chan workItem

	ready atomic.Bool

	factories []dynamicinformer.DynamicSharedInformerFactory
	syncs     []cache.InformerSynced

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Options configures a Collector instance.
type Options struct {
	Config       Config
	Kube         k8s.Client
	Queue        queue.Queue
	AgentMeta    *commonv1.AgentMetadata
	Logger       *logger.Logger
	Registerer   prometheus.Registerer
	WriteTimeout time.Duration
}

// New constructs a state Collector.
func New(opts Options) (*Collector, error) {
	if opts.Kube == nil {
		return nil, errors.New("kubernetes client is required")
	}
	if opts.Queue == nil {
		return nil, errors.New("queue is required")
	}

	cfg := opts.Config
	cfg.ApplyDefaults()
	if !cfg.IsEnabled() {
		return nil, errors.New("state collection is disabled")
	}
	if !cfg.HasWatchResources() {
		return nil, errors.New("state collection has no resources configured")
	}

	log := opts.Logger
	if log == nil {
		log = logger.New(componentName)
	}
	log = log.With("component", componentName)

	metrics, err := newMetrics(opts.Registerer)
	if err != nil {
		return nil, fmt.Errorf("init metrics: %w", err)
	}

	writeTimeout := cfg.WriteTimeout
	if opts.WriteTimeout > 0 {
		writeTimeout = opts.WriteTimeout
	}

	meta := opts.AgentMeta
	if meta == nil {
		meta = &commonv1.AgentMetadata{}
	}

	dyn := opts.Kube.Dynamic()
	if dyn == nil {
		return nil, errors.New("kubernetes dynamic client is required")
	}

	return &Collector{
		cfg:       cfg,
		dynamic:   dyn,
		writer:    &queueWriter{q: opts.Queue, timeout: writeTimeout},
		log:       log,
		metrics:   metrics,
		agentMeta: proto.Clone(meta).(*commonv1.AgentMetadata),
		workCh:    make(chan workItem, cfg.BufferSize),
	}, nil
}

// Name returns the collector identifier.
func (c *Collector) Name() string {
	return componentName
}

// Ready reports whether informer caches have synced.
func (c *Collector) Ready() bool {
	return c != nil && c.ready.Load()
}

// Start launches informers and worker goroutines.
func (c *Collector) Start(ctx context.Context) error {
	if c.cancel != nil {
		return errors.New("state watcher already started")
	}
	c.runCtx, c.cancel = context.WithCancel(ctx)

	c.wg.Add(c.cfg.WorkerCount)
	for i := 0; i < c.cfg.WorkerCount; i++ {
		go func() {
			defer c.wg.Done()
			c.runWorker(c.runCtx)
		}()
	}

	if err := c.startInformers(c.runCtx); err != nil {
		c.cancel()
		c.wg.Wait()
		c.cancel = nil
		c.runCtx = nil
		return err
	}

	c.log.Info("state watcher started",
		logger.F("workers", c.cfg.WorkerCount),
		logger.F("buffer", c.cfg.BufferSize),
		logger.F("resync_period", c.cfg.ResyncPeriod.String()),
	)
	return nil
}

// Stop shuts down informers and workers.
func (c *Collector) Stop(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	c.ready.Store(false)

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		c.cancel = nil
		c.runCtx = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Collector) startInformers(ctx context.Context) error {
	rules := c.watchRules()
	if len(rules) == 0 {
		return errors.New("no state watch rules configured")
	}

	emitter := newEventEmitter(c.workCh, c.metrics)
	var allSyncs []cache.InformerSynced
	seenInformers := make(map[string]struct{})
	var syncedLabels []string

	for _, rule := range rules {
		resync := rule.ResolveResyncPeriod(c.cfg.ResyncPeriod)
		tweak := func(lo *metav1.ListOptions) {
			listOpts := rule.WatchListOptions()
			lo.LabelSelector = listOpts.LabelSelector
			lo.FieldSelector = listOpts.FieldSelector
		}

		factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamic,
			resync,
			rule.Namespace,
			tweak,
		)

		descriptors, err := ruleDescriptors(rule)
		if err != nil {
			return fmt.Errorf("state rule %q: %w", rule.ID, err)
		}

		for _, desc := range descriptors {
			key := informerKey(rule.Namespace, desc)
			if _, dup := seenInformers[key]; dup {
				continue
			}
			seenInformers[key] = struct{}{}

			informer := factory.ForResource(desc.GVR).Informer()
			if _, err := informer.AddEventHandler(emitter.handlersFor(desc)); err != nil {
				return fmt.Errorf("register event handler for %s: %w", desc.MetricLabel, err)
			}
			allSyncs = append(allSyncs, informer.HasSynced)
			c.metrics.setCacheSynced(desc.MetricLabel, false)
			syncedLabels = append(syncedLabels, desc.MetricLabel)
		}

		factory.Start(ctx.Done())
		c.factories = append(c.factories, factory)
	}

	c.syncs = allSyncs
	if len(allSyncs) == 0 {
		return errors.New("no informers registered for configured resources")
	}

	if !cache.WaitForCacheSync(ctx.Done(), allSyncs...) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("state watcher cache sync: %w", err)
		}
		return errors.New("state watcher cache sync failed")
	}

	for _, label := range syncedLabels {
		c.metrics.setCacheSynced(label, true)
	}
	c.ready.Store(true)
	c.log.Info("state watcher caches synced", logger.F("informers", len(allSyncs)))
	return nil
}

func informerKey(namespace string, desc k8sresource.Descriptor) string {
	return namespace + "/" + desc.GVR.String()
}

func ruleDescriptors(rule pkgconfig.StateNamespaceRule) ([]k8sresource.Descriptor, error) {
	if len(rule.Resources) == 0 {
		return nil, fmt.Errorf("resources must not be empty")
	}
	seen := make(map[string]struct{})
	out := make([]k8sresource.Descriptor, 0, len(rule.Resources))
	for _, name := range rule.Resources {
		desc, err := k8sresource.Parse(name)
		if err != nil {
			return nil, err
		}
		key := desc.GVR.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, desc)
	}
	return out, nil
}

func (c *Collector) watchRules() []pkgconfig.StateNamespaceRule {
	if len(c.cfg.Rules) > 0 {
		return c.cfg.Rules
	}
	if len(c.cfg.Namespaces) == 0 {
		return []pkgconfig.StateNamespaceRule{{Namespace: "", Resources: []string{"pods", "services", "deployments", "secrets"}}}
	}
	out := make([]pkgconfig.StateNamespaceRule, 0, len(c.cfg.Namespaces))
	for _, ns := range c.cfg.Namespaces {
		out = append(out, pkgconfig.StateNamespaceRule{
			Namespace: ns,
			Resources: []string{"pods", "services", "deployments", "secrets"},
		})
	}
	return out
}

func (c *Collector) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-c.workCh:
			if !ok {
				return
			}
			c.metrics.setQueueDepth(len(c.workCh))
			start := time.Now()
			if err := c.processItem(ctx, item); err != nil {
				c.metrics.incDropped(item.desc.MetricLabel, dropReason(err))
			} else {
				c.metrics.incEvent(item.desc.MetricLabel, item.eventType)
			}
			c.metrics.observeProcessing(time.Since(start))
			c.metrics.setQueueDepth(len(c.workCh))
		}
	}
}

func (c *Collector) processItem(ctx context.Context, item workItem) error {
	if item.object == nil {
		return errors.New("work item object is nil")
	}

	objectJSON, err := MarshalObjectJSON(item.object, item.desc.GVR.Resource)
	if err != nil {
		return fmt.Errorf("marshal object: %w", err)
	}

	namespace, name, _, _ := ResourceMeta(item.object)
	eventType := protoEventType(item.eventType)
	labels := ObjectLabels(item.object)

	event := &agentv1.StateEvent{
		Type:        eventType,
		Kind:        item.desc.ProtoKind,
		Name:        name,
		Namespace:   namespace,
		Object:      objectJSON,
		Timestamp:   time.Now().UTC().UnixMilli(),
		Labels:      labels,
		ApiVersion:  item.desc.APIVersion(),
		Resource:    item.desc.GVR.Resource,
	}

	msg := &agentv1.AgentMessage{
		MessageId: uuid.NewString(),
		Meta:      c.metaSnapshot(time.Now().UTC()),
		Payload: &agentv1.AgentMessage_State{
			State: event,
		},
	}

	if err := c.writer.Write(ctx, msg); err != nil {
		return err
	}
	return nil
}

func protoEventType(eventType string) agentv1.EventType {
	switch eventType {
	case eventAdded:
		return agentv1.EventType_EVENT_TYPE_ADDED
	case eventModified:
		return agentv1.EventType_EVENT_TYPE_MODIFIED
	case eventDeleted:
		return agentv1.EventType_EVENT_TYPE_DELETED
	default:
		return agentv1.EventType_EVENT_TYPE_UNSPECIFIED
	}
}

func (c *Collector) metaSnapshot(ts time.Time) *commonv1.AgentMetadata {
	meta := proto.Clone(c.agentMeta).(*commonv1.AgentMetadata)
	if meta == nil {
		meta = &commonv1.AgentMetadata{}
	}
	meta.Timestamp = ts.UnixMilli()
	return meta
}

func dropReason(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "queue_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "queue_error"
}
