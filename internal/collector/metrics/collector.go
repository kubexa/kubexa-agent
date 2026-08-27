// Package metrics scrapes Kubernetes Metrics API and custom Prometheus endpoints.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/queue"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const componentName = "metrics-scraper"

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
		return fmt.Errorf("enqueue metrics message: %w", err)
	}
	return nil
}

// Collector schedules and runs Kubernetes and custom metrics scrapers.
type Collector struct {
	cfg       Config
	writer    Writer
	log       *logger.Logger
	metrics   *scraperMetrics
	health    *ScrapeHealth
	agentMeta *commonv1.AgentMetadata

	k8s    *k8sMetricsScraper
	custom *customScraper
	// kube is the raw client the kube-state-metrics presence probe uses. It is
	// stored separately from k8s (the Metrics API scraper) because the probe
	// is required whenever KubeState.ProbeService is set, independent of
	// whether any Kubernetes Metrics API rule is configured at all.
	kube k8s.Client

	customFilters   map[string]*MetricFilter
	customFiltersMu sync.RWMutex

	dynamic       *dynamicProvider
	dynamicCancel map[string]context.CancelFunc
	dynamicMu     sync.Mutex

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	rng *rand.Rand
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

// New constructs a metrics Collector.
func New(opts Options) (*Collector, error) {
	if opts.Kube == nil && opts.Config.HasKubeMetricsRules() {
		return nil, errors.New("kubernetes client is required when kubernetes metrics rules are configured")
	}
	if opts.Queue == nil {
		return nil, errors.New("queue is required")
	}

	cfg := opts.Config
	cfg.ApplyDefaults()
	if !cfg.IsEnabled() {
		return nil, errors.New("metrics collection is disabled")
	}
	if cfg.KubeState.Enabled && cfg.KubeState.ProbeService && opts.Kube == nil {
		return nil, errors.New("kubernetes client is required to probe for kube-state-metrics")
	}

	log := opts.Logger
	if log == nil {
		log = logger.New(componentName)
	}
	log = log.With("component", componentName)

	scraperMetrics, err := newScraperMetrics(opts.Registerer)
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

	customFilters, err := buildCustomFilters(cfg.CustomTargets)
	if err != nil {
		return nil, err
	}
	if customFilters == nil {
		customFilters = make(map[string]*MetricFilter)
	}

	c := &Collector{
		cfg:           cfg,
		writer:        &queueWriter{q: opts.Queue, timeout: writeTimeout},
		log:           log,
		metrics:       scraperMetrics,
		health:        newScrapeHealth(),
		agentMeta:     proto.Clone(meta).(*commonv1.AgentMetadata),
		customFilters: customFilters,
		kube:          opts.Kube,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	if opts.Kube != nil && cfg.HasKubeMetricsRules() {
		c.k8s = newK8sMetricsScraper(opts.Kube, scraperMetrics)
		c.k8s.setLogger(log)
	}
	if len(cfg.CustomTargets) > 0 || cfg.KubeState.Enabled || len(cfg.DynamicTargets.Templates) > 0 {
		c.custom = newCustomScraper(scraperMetrics)
		c.custom.setLogger(log)
	}
	if len(cfg.DynamicTargets.Templates) > 0 {
		if opts.Kube == nil {
			return nil, errors.New("kubernetes client is required when dynamic scrape targets are configured")
		}
		c.dynamic = newDynamicProvider(opts.Kube, cfg.DynamicTargets, log)
		c.dynamicCancel = make(map[string]context.CancelFunc)
	}
	return c, nil
}

// Name returns the collector identifier.
func (c *Collector) Name() string {
	return componentName
}

// Health returns the collector's scrape health. The heartbeat reports it; it
// is not derived from the Prometheus counters, which live on a listener
// nothing outside the cluster reads.
func (c *Collector) Health() *ScrapeHealth { return c.health }

// Start launches scraper goroutines with independent tickers.
func (c *Collector) Start(ctx context.Context) error {
	if c.cancel != nil {
		return errors.New("metrics scraper already started")
	}
	c.runCtx, c.cancel = context.WithCancel(ctx)

	if c.k8s != nil {
		for _, rule := range c.cfg.KubernetesMetrics.Rules {
			rule := rule
			if rule.CollectPods {
				c.wg.Add(1)
				go func() {
					defer c.wg.Done()
					c.runRulePodScraper(c.runCtx, rule)
				}()
			}
			if rule.CollectNodes {
				c.wg.Add(1)
				go func() {
					defer c.wg.Done()
					c.runRuleNodeScraper(c.runCtx, rule)
				}()
			}
		}
	}

	if len(c.cfg.CustomTargets) > 0 {
		c.health.SetTargetCount("custom", len(c.cfg.CustomTargets))
	}

	for _, target := range c.cfg.CustomTargets {
		target := target
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.runCustomTarget(c.runCtx, target)
		}()
	}

	if c.dynamic != nil {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.runDynamicTargets(c.runCtx)
		}()
	}

	if c.cfg.KubeState.Enabled {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.runKubeStateTarget(c.runCtx)
		}()
	}

	c.log.Info("metrics scraper started",
		logger.F("kube_rules", len(c.cfg.KubernetesMetrics.Rules)),
		logger.F("custom_targets", len(c.cfg.CustomTargets)),
		logger.F("pod_interval", c.cfg.KubernetesMetrics.PodInterval.String()),
		logger.F("node_interval", c.cfg.KubernetesMetrics.NodeInterval.String()),
	)
	return nil
}

// Stop cancels scrapers and waits for goroutines to exit.
func (c *Collector) Stop(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()

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

func (c *Collector) runRulePodScraper(ctx context.Context, rule KubeMetricsRule) {
	interval := rule.PodInterval
	c.waitStartupJitter(ctx, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	backoff := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if backoff > 0 {
			if !c.wait(ctx, backoff) {
				return
			}
		}

		events, retryAfter, err := c.k8s.ScrapePodsForRuleWithBackoff(ctx, rule)
		if err != nil {
			backoff = chooseBackoff(retryAfter, backoff)
			continue
		}
		backoff = 0
		c.publishKubeMetrics(ctx, events)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Collector) runRuleNodeScraper(ctx context.Context, rule KubeMetricsRule) {
	interval := rule.NodeInterval
	c.waitStartupJitter(ctx, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	backoff := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if backoff > 0 {
			if !c.wait(ctx, backoff) {
				return
			}
		}

		events, retryAfter, err := c.k8s.ScrapeNodesForRuleWithBackoff(ctx, rule)
		if err != nil {
			backoff = chooseBackoff(retryAfter, backoff)
			continue
		}
		backoff = 0
		c.publishKubeMetrics(ctx, events)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Collector) runCustomTarget(ctx context.Context, target ScrapeTarget) {
	interval := target.Interval
	c.waitStartupJitter(ctx, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	c.customFiltersMu.RLock()
	filter := c.customFilters[targetKey(target)]
	c.customFiltersMu.RUnlock()
	targetName := targetLabel(target)
	backoff := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if backoff > 0 {
			if !c.wait(ctx, backoff) {
				return
			}
		}

		result, err := c.custom.ScrapeTarget(ctx, target, filter)
		if err != nil {
			backoff = nextBackoff(backoff)
			c.health.RecordFailure(healthKind(target), targetName, err)
			c.log.Warn("custom metrics scrape failed",
				logger.F("target", targetName),
				logger.F("url", target.URL),
				logger.F("error", err.Error()),
				logger.F("backoff", backoff.String()),
			)
		} else {
			backoff = 0
			families, droppedSamples := applySampleBudget(result.Families, c.cfg.MaxSamplesPerScrape)
			if droppedSamples > 0 {
				c.health.RecordCardinalityDrop(healthKind(target), droppedSamples)
				c.log.Warn("scrape exceeded the sample budget; whole families dropped",
					logger.F("target", targetLabel(target)),
					logger.F("budget", c.cfg.MaxSamplesPerScrape),
					logger.F("dropped_samples", droppedSamples),
				)
			}
			result.Families = families
			c.health.RecordSuccess(healthKind(target), targetName, countSamples(result.Families))
			if err := c.publishPrometheusMetrics(ctx, target, result); err != nil {
				c.log.Warn("publish custom metrics failed",
					logger.F("target", targetName),
					logger.F("error", err.Error()),
				)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runDynamicTargets refreshes generated targets on an interval, starting a
// scraper goroutine for each new target and cancelling the goroutine of each
// target that is gone.
//
// The first refresh runs immediately rather than after one interval: a restart
// would otherwise leave every generated target unscraped for the whole refresh
// window, which reads on the graphs as an outage of the thing being measured.
func (c *Collector) runDynamicTargets(ctx context.Context) {
	ticker := time.NewTicker(c.dynamic.interval)
	defer ticker.Stop()

	for {
		added, removed, err := c.dynamic.refresh(ctx)
		if err != nil {
			c.log.Warn("dynamic scrape target refresh failed",
				logger.F("error", err.Error()))
		} else {
			for _, target := range removed {
				c.stopDynamicTarget(target)
			}
			for _, target := range added {
				c.startDynamicTarget(ctx, target)
			}
			if len(added) > 0 || len(removed) > 0 {
				c.log.Info("dynamic scrape targets changed",
					logger.F("added", len(added)),
					logger.F("removed", len(removed)),
					logger.F("total", len(c.dynamic.current)),
				)
			}

			counts := map[string]int{}
			for _, t := range c.dynamic.current {
				counts[healthKind(t)]++
			}
			for _, tpl := range c.dynamic.templates {
				c.health.SetTargetCount(tpl.Kind, counts[tpl.Kind])
			}
		}

		select {
		case <-ctx.Done():
			c.stopAllDynamicTargets()
			return
		case <-ticker.C:
		}
	}
}

func (c *Collector) startDynamicTarget(ctx context.Context, target ScrapeTarget) {
	filter, err := NewMetricFilter(target.MetricAllowlist, target.MetricDenylist)
	if err != nil {
		// Validation refuses a bad pattern at startup, so reaching here means
		// a template built one. Refusing this one target is right; refusing
		// every target would take the whole node fleet down with it.
		c.log.Warn("dynamic scrape target filter rejected",
			logger.F("target", targetLabel(target)),
			logger.F("error", err.Error()))
		return
	}

	targetCtx, cancel := context.WithCancel(ctx)

	c.dynamicMu.Lock()
	if prev, running := c.dynamicCancel[targetIdentity(target)]; running {
		// Should not happen -- diffTargets only reports genuinely new targets
		// -- but a leaked goroutine here scrapes forever.
		prev()
	}
	c.dynamicCancel[targetIdentity(target)] = cancel
	c.customFiltersMu.Lock()
	c.customFilters[targetKey(target)] = filter
	c.customFiltersMu.Unlock()
	c.dynamicMu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runCustomTarget(targetCtx, target)
	}()
}

func (c *Collector) stopDynamicTarget(target ScrapeTarget) {
	c.dynamicMu.Lock()
	cancel, running := c.dynamicCancel[targetIdentity(target)]
	delete(c.dynamicCancel, targetIdentity(target))
	c.customFiltersMu.Lock()
	delete(c.customFilters, targetKey(target))
	c.customFiltersMu.Unlock()
	c.dynamicMu.Unlock()

	// Outside both locks above: a removed target never scrapes again, so
	// nothing else will ever call RecordSuccess to clear a failure it left
	// behind. Without this, a target that failed right before removal reports
	// as a standing outage for the rest of the process's life.
	c.health.ClearTarget(healthKind(target), targetLabel(target))

	if running {
		cancel()
	}
}

func (c *Collector) stopAllDynamicTargets() {
	c.dynamicMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.dynamicCancel))
	for id, cancel := range c.dynamicCancel {
		cancels = append(cancels, cancel)
		delete(c.dynamicCancel, id)
	}
	c.dynamicMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

func (c *Collector) publishKubeMetrics(ctx context.Context, events []*agentv1.MetricsEvent) {
	for _, event := range events {
		if event == nil {
			continue
		}
		msg := &agentv1.AgentMessage{
			MessageId: uuid.NewString(),
			Meta:      c.metaSnapshot(TimestampOrNow(event.Timestamp)),
			Payload: &agentv1.AgentMessage_KubeMetrics{
				KubeMetrics: event,
			},
		}
		if err := c.writer.Write(ctx, msg); err != nil {
			c.metrics.incDropped()
			c.log.Warn("enqueue kube metrics failed", logger.F("error", err.Error()))
		}
	}
}

func (c *Collector) publishPrometheusMetrics(ctx context.Context, target ScrapeTarget, result customScrapeResult) error {
	events, err := BuildPrometheusEvents(target, result)
	if err != nil {
		return err
	}
	for _, event := range events {
		msg := &agentv1.AgentMessage{
			MessageId: uuid.NewString(),
			Meta:      c.metaSnapshot(result.Scraped),
			Payload: &agentv1.AgentMessage_PrometheusMetrics{
				PrometheusMetrics: event,
			},
		}
		if err := c.writer.Write(ctx, msg); err != nil {
			c.metrics.incDropped()
			return err
		}
	}
	return nil
}

func (c *Collector) metaSnapshot(ts time.Time) *commonv1.AgentMetadata {
	meta := proto.Clone(c.agentMeta).(*commonv1.AgentMetadata)
	if meta == nil {
		meta = &commonv1.AgentMetadata{}
	}
	meta.Timestamp = ts.UnixMilli()
	return meta
}

func (c *Collector) waitStartupJitter(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	jitter := time.Duration(c.rng.Int63n(int64(interval)))
	if jitter == 0 {
		return
	}
	_ = c.wait(ctx, jitter)
}

func (c *Collector) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func buildCustomFilters(targets []ScrapeTarget) (map[string]*MetricFilter, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	out := make(map[string]*MetricFilter, len(targets))
	for _, target := range targets {
		filter, err := NewMetricFilter(target.MetricAllowlist, target.MetricDenylist)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", targetLabel(target), err)
		}
		out[targetKey(target)] = filter
	}
	return out, nil
}

func targetKey(target ScrapeTarget) string {
	if target.Name != "" {
		return target.Name
	}
	return target.URL
}

func chooseBackoff(recommended, current time.Duration) time.Duration {
	if recommended > 0 {
		return recommended
	}
	return nextBackoff(current)
}
