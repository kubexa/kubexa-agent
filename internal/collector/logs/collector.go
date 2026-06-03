// Package logs collects Kubernetes pod logs and buffers them for export.
package logs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/queue"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const componentName = "log-collector"

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
		return fmt.Errorf("enqueue log message: %w", err)
	}
	return nil
}

// Collector streams pod logs and writes structured events to the buffer queue.
type Collector struct {
	cfg       Config
	kube      k8s.Client
	writer    Writer
	log       *logger.Logger
	metrics   *metrics
	agentMeta *commonv1.AgentMetadata
	sem       *semaphore.Weighted

	streamsMu    sync.Mutex
	streams      map[streamKey]*streamHandle
	checkpoints  checkpoint.Store

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sleep func(context.Context, time.Duration) error
}

// Options configures a Collector instance.
type Options struct {
	Config        Config
	Kube          k8s.Client
	Queue         queue.Queue
	AgentMeta     *commonv1.AgentMetadata
	Logger        *logger.Logger
	Registerer    prometheus.Registerer
	WriteTimeout    time.Duration
	Sleep           func(context.Context, time.Duration) error
	CheckpointStore checkpoint.Store
}

// New constructs a log Collector.
func New(opts Options) (*Collector, error) {
	if opts.Kube == nil {
		return nil, errors.New("kubernetes client is required")
	}
	if opts.Queue == nil {
		return nil, errors.New("queue is required")
	}

	cfg := opts.Config
	cfg.ApplyDefaults()
	if !cfg.Enabled() {
		return nil, errors.New("log collection is disabled")
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

	sleep := opts.Sleep
	if sleep == nil {
		sleep = defaultSleep
	}

	var cpStore checkpoint.Store
	switch {
	case opts.CheckpointStore != nil:
		cpStore = opts.CheckpointStore
	case cfg.CheckpointDir != "":
		cpStore, err = checkpoint.Open(cfg.CheckpointDir)
		if err != nil {
			return nil, fmt.Errorf("open checkpoint store: %w", err)
		}
		log.Info("log checkpoint store enabled", logger.F("checkpoint_dir", cfg.CheckpointDir))
	}

	return &Collector{
		cfg:         cfg,
		kube:        opts.Kube,
		writer:      &queueWriter{q: opts.Queue, timeout: writeTimeout},
		log:         log,
		metrics:     metrics,
		agentMeta:   proto.Clone(meta).(*commonv1.AgentMetadata),
		sem:         semaphore.NewWeighted(cfg.MaxConcurrentStreams),
		streams:     make(map[streamKey]*streamHandle),
		checkpoints: cpStore,
		sleep:       sleep,
	}, nil
}

// Name returns the collector identifier.
func (c *Collector) Name() string {
	return componentName
}

// Start begins pod discovery and log streaming.
func (c *Collector) Start(ctx context.Context) error {
	if c.cancel != nil {
		return errors.New("log collector already started")
	}
	c.runCtx, c.cancel = context.WithCancel(ctx)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run(c.runCtx)
	}()
	return nil
}

// Stop cancels streaming and waits for goroutines to exit.
func (c *Collector) Stop(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	c.stopAllStreams()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		c.closeCheckpoints(ctx)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Collector) closeCheckpoints(ctx context.Context) {
	if c.checkpoints == nil {
		return
	}
	store := c.checkpoints
	c.checkpoints = nil
	if err := store.Flush(ctx); err != nil {
		c.log.Warn("checkpoint flush failed", logger.F("error", err.Error()))
		c.metrics.incCheckpointError("flush")
	}
	if err := store.Close(); err != nil {
		c.log.Warn("checkpoint store close failed", logger.F("error", err.Error()))
	}
}

func (c *Collector) run(ctx context.Context) {
	c.log.Info("log collector started",
		logger.F("rules", len(c.cfg.Rules)),
		logger.F("resync_period", c.cfg.ResyncPeriod.String()),
		logger.F("max_streams", c.cfg.MaxConcurrentStreams),
	)

	for _, rule := range c.cfg.Rules {
		rule := rule
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.watchRule(ctx, rule)
		}()
	}

	c.resync(ctx)

	ticker := time.NewTicker(c.cfg.ResyncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.log.Info("log collector stopping")
			return
		case <-ticker.C:
			c.resync(ctx)
		}
	}
}

func (c *Collector) listPods(ctx context.Context, namespace string, view ruleView) ([]corev1.Pod, error) {
	listOpts := metav1.ListOptions{
		LabelSelector: view.labelSel,
		FieldSelector: view.fieldSel,
	}
	ns := namespace
	if ns == "" {
		ns = metav1.NamespaceAll
	}
	list, err := c.kube.Pods(ns).List(ctx, listOpts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func matchesPodName(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if name == pattern {
			return true
		}
	}
	return false
}

func (c *Collector) abortReservation(key streamKey) {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	handle, ok := c.streams[key]
	if !ok || !handle.starting {
		return
	}
	delete(c.streams, key)
}

// reconcileStreams starts streams in desired and optionally removes streams missing from desired.
func (c *Collector) reconcileStreams(ctx context.Context, desired map[streamKey]streamTarget, prune bool) {
	var toStart []streamTarget

	c.streamsMu.Lock()
	if prune {
		for key, handle := range c.streams {
			if _, ok := desired[key]; !ok {
				handle.cancel()
				delete(c.streams, key)
			}
		}
	}
	for key, target := range desired {
		if _, exists := c.streams[key]; exists {
			continue
		}
		c.streams[key] = &streamHandle{
			cancel:   func() {},
			starting: true,
		}
		toStart = append(toStart, target)
	}
	c.streamsMu.Unlock()

	for _, target := range toStart {
		if err := c.sem.Acquire(ctx, 1); err != nil {
			c.abortReservation(target.key)
			if !errors.Is(err, context.Canceled) {
				c.log.Warn("stream semaphore acquire failed", logger.F("error", err.Error()))
			}
			continue
		}

		c.wg.Add(1)
		go func(t streamTarget) {
			defer c.wg.Done()
			c.runStream(ctx, t)
		}(target)
	}

	c.updateActiveStreamsMetric()
}

func (c *Collector) stopAllStreams() {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	for key, handle := range c.streams {
		handle.cancel()
		delete(c.streams, key)
	}
	c.updateActiveStreamsMetricLocked()
}

func (c *Collector) releaseStream(key streamKey) {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	delete(c.streams, key)
	c.updateActiveStreamsMetricLocked()
}

func (c *Collector) updateActiveStreamsMetric() {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	c.updateActiveStreamsMetricLocked()
}

func (c *Collector) updateActiveStreamsMetricLocked() {
	c.metrics.setActiveStreams(len(c.streams))
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
