package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "agent"

// scraperMetrics holds Prometheus instrumentation for the metrics scraper.
type scraperMetrics struct {
	scrapesTotal      *prometheus.CounterVec
	scrapeDuration    *prometheus.HistogramVec
	collectedTotal    *prometheus.CounterVec
	droppedTotal      prometheus.Counter
	lastScrapeTS      *prometheus.GaugeVec
}

func newScraperMetrics(reg prometheus.Registerer) (*scraperMetrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &scraperMetrics{
		scrapesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "metrics_scraper_scrapes_total",
				Help:      "Total scrape attempts by the metrics scraper.",
			},
			[]string{"source", "target", "status"},
		),
		scrapeDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Name:      "metrics_scraper_scrape_duration_seconds",
				Help:      "Duration of metrics scraper scrape operations.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"source", "target"},
		),
		collectedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "metrics_scraper_metrics_collected_total",
				Help:      "Total metric samples collected by the metrics scraper.",
			},
			[]string{"source"},
		),
		droppedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "metrics_scraper_dropped_total",
				Help:      "Total metric messages dropped by the metrics scraper.",
			},
		),
		lastScrapeTS: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Name:      "metrics_scraper_last_scrape_timestamp",
				Help:      "Unix timestamp of the last successful scrape per target.",
			},
			[]string{"source", "target"},
		),
	}

	collectors := []prometheus.Collector{
		m.scrapesTotal,
		m.scrapeDuration,
		m.collectedTotal,
		m.droppedTotal,
		m.lastScrapeTS,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register metrics scraper metric: %w", err)
		}
	}
	return m, nil
}

func (m *scraperMetrics) incScrape(source, target, status string) {
	if m == nil {
		return
	}
	m.scrapesTotal.WithLabelValues(source, target, status).Inc()
}

func (m *scraperMetrics) observeScrape(source, target string, d time.Duration) {
	if m == nil {
		return
	}
	m.scrapeDuration.WithLabelValues(source, target).Observe(d.Seconds())
}

func (m *scraperMetrics) addCollected(source string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.collectedTotal.WithLabelValues(source).Add(float64(n))
}

func (m *scraperMetrics) incDropped() {
	if m == nil {
		return
	}
	m.droppedTotal.Inc()
}

func (m *scraperMetrics) setLastScrape(source, target string, ts time.Time) {
	if m == nil {
		return
	}
	m.lastScrapeTS.WithLabelValues(source, target).Set(float64(ts.Unix()))
}
