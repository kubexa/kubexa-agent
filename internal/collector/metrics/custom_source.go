package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const (
	customSourceLabel   = "custom"
	maxScrapeRetries    = 3
	retryDelay          = 1 * time.Second
	initialBackoff      = 5 * time.Second
	maxBackoff          = 2 * time.Minute
)

// customScraper scrapes Prometheus exposition endpoints over HTTP.
type customScraper struct {
	pool    *httpClientPool
	metrics *scraperMetrics
	log     *logger.Logger
}

type customScrapeResult struct {
	Families []ParsedFamily
	Scraped  time.Time
}

// newCustomScraper constructs a custom endpoint scraper.
func newCustomScraper(metrics *scraperMetrics) *customScraper {
	return &customScraper{
		pool:    newHTTPClientPool(),
		metrics: metrics,
	}
}

func (s *customScraper) setLogger(log *logger.Logger) {
	s.log = log
}

// ScrapeTarget performs one scrape of the configured target with retries and backoff semantics.
func (s *customScraper) ScrapeTarget(ctx context.Context, target ScrapeTarget, filter *MetricFilter) (customScrapeResult, error) {
	client, err := s.pool.clientFor(target.TLSConfig)
	if err != nil {
		return customScrapeResult{}, err
	}

	targetName := targetLabel(target)
	var lastErr error
	for attempt := 1; attempt <= maxScrapeRetries; attempt++ {
		start := time.Now()
		body, status, err := s.fetch(ctx, client, target)
		elapsed := time.Since(start)
		s.metrics.observeScrape(customSourceLabel, targetName, elapsed)

		if err != nil {
			lastErr = err
			if attempt < maxScrapeRetries && isRetryableFetchError(err) {
				if waitErr := sleep(ctx, retryDelay); waitErr != nil {
					return customScrapeResult{}, waitErr
				}
				continue
			}
			s.metrics.incScrape(customSourceLabel, targetName, "error")
			return customScrapeResult{}, lastErr
		}

		switch {
		case status >= 500:
			lastErr = fmt.Errorf("scrape %q: HTTP %d", target.URL, status)
			s.metrics.incScrape(customSourceLabel, targetName, "error")
			if attempt < maxScrapeRetries {
				if waitErr := sleep(ctx, retryDelay); waitErr != nil {
					return customScrapeResult{}, waitErr
				}
				continue
			}
			return customScrapeResult{}, lastErr
		case status >= 400:
			s.metrics.incScrape(customSourceLabel, targetName, "error")
			if s.log != nil {
				s.log.Warn("custom metrics scrape client error",
					logger.F("target", targetName),
					logger.F("url", target.URL),
					logger.F("status", status),
				)
			}
			return customScrapeResult{}, fmt.Errorf("scrape %q: HTTP %d", target.URL, status)
		}

		families, err := ParsePrometheusText(body)
		if err != nil {
			s.metrics.incScrape(customSourceLabel, targetName, "error")
			return customScrapeResult{}, fmt.Errorf("parse prometheus text from %q: %w", target.URL, err)
		}
		families = FilterFamilies(families, filter)
		for i := range families {
			families[i].Metrics = applyTargetLabels(families[i].Metrics, target.Labels)
		}

		scraped := time.Now().UTC()
		s.metrics.incScrape(customSourceLabel, targetName, "success")
		s.metrics.setLastScrape(customSourceLabel, targetName, scraped)
		s.metrics.addCollected(customSourceLabel, countSamples(families))
		return customScrapeResult{Families: families, Scraped: scraped}, nil
	}
	return customScrapeResult{}, lastErr
}

func (s *customScraper) fetch(ctx context.Context, client *http.Client, target ScrapeTarget) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, target.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.URL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create scrape request: %w", err)
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4")

	if target.BearerTokenPath != "" {
		token, err := os.ReadFile(target.BearerTokenPath)
		if err != nil {
			return nil, 0, fmt.Errorf("read bearer token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("execute scrape request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	const maxBody = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read scrape response: %w", err)
	}
	return body, resp.StatusCode, nil
}

func applyTargetLabels(metrics []ParsedMetric, extra map[string]string) []ParsedMetric {
	if len(extra) == 0 {
		return metrics
	}
	out := make([]ParsedMetric, len(metrics))
	for i, m := range metrics {
		out[i] = ParsedMetric{
			Labels:    MergeLabels(m.Labels, extra),
			Value:     m.Value,
			Timestamp: m.Timestamp,
		}
	}
	return out
}

func countSamples(families []ParsedFamily) int {
	n := 0
	for _, fam := range families {
		n += len(fam.Metrics)
	}
	return n
}

func targetLabel(target ScrapeTarget) string {
	if target.Name != "" {
		return target.Name
	}
	return target.URL
}

func isRetryableFetchError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// httpClientPool reuses http.Client instances per TLS configuration.
type httpClientPool struct {
	mu      sync.Mutex
	clients map[tlsPoolKey]*http.Client
}

type tlsPoolKey struct {
	insecure bool
	caFile   string
}

func newHTTPClientPool() *httpClientPool {
	return &httpClientPool{
		clients: make(map[tlsPoolKey]*http.Client),
	}
}

func (p *httpClientPool) clientFor(cfg TLSConfig) (*http.Client, error) {
	key := tlsPoolKey{
		insecure: cfg.InsecureSkipVerify,
		caFile:   cfg.CAFile,
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[key]; ok {
		return client, nil
	}

	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 2
	transport.TLSClientConfig = tlsCfg

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	p.clients[key] = client
	return client, nil
}

func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	if cfg.CAFile == "" {
		return tlsCfg, nil
	}
	data, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", cfg.CAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("parse CA bundle %q: no certificates found", cfg.CAFile)
	}
	tlsCfg.RootCAs = pool
	return tlsCfg, nil
}

// BuildPrometheusEvents converts scrape results into queue events (one per family).
func BuildPrometheusEvents(target ScrapeTarget, result customScrapeResult) ([]*agentv1.PrometheusMetricsEvent, error) {
	events := make([]*agentv1.PrometheusMetricsEvent, 0, len(result.Families))
	for _, fam := range result.Families {
		jsonPayload, err := FamilyJSON(fam)
		if err != nil {
			return nil, err
		}
		events = append(events, &agentv1.PrometheusMetricsEvent{
			TargetUrl:        target.URL,
			MetricFamilyJson: jsonPayload,
			ScrapedAt:        result.Scraped.UnixMilli(),
		})
	}
	return events, nil
}

func nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return initialBackoff
	}
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
