package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

const shutdownTimeout = 5 * time.Second

// Server exposes Prometheus metrics over HTTP.
type Server struct {
	addr     string
	gatherer prometheus.Gatherer
	log      *logger.Logger
	httpSrv  *http.Server
}

// NewServer constructs a metrics HTTP server bound to addr.
func NewServer(addr string, reg prometheus.Gatherer, log *logger.Logger) *Server {
	if reg == nil {
		reg = prometheus.DefaultGatherer
	}
	if log == nil {
		log = logger.New("metrics")
	}

	mux := http.NewServeMux()
	srv := &Server{
		addr:     addr,
		gatherer: reg,
		log:      log,
	}

	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/metrics/json", http.HandlerFunc(srv.serveJSON))

	srv.httpSrv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return srv
}

// Run starts the metrics server and blocks until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	if s == nil || s.httpSrv == nil {
		return fmt.Errorf("metrics server: not initialized")
	}

	s.log.Info("metrics server listening", logger.F("addr", s.addr))

	errCh := make(chan error, 1)
	go func() {
		err := s.httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("metrics server shutdown: %w", err)
		}
		if err := <-errCh; err != nil {
			return fmt.Errorf("metrics server: %w", err)
		}
		return ctx.Err()
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("metrics server: %w", err)
		}
		return nil
	}
}

func (s *Server) serveJSON(w http.ResponseWriter, r *http.Request) {
	mfs, err := s.gatherer.Gather()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	payload, err := metricFamiliesToJSON(mfs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(payload); err != nil {
		s.log.Warn("failed to write metrics json response", logger.F("error", err))
	}
}

type jsonMetricFamily struct {
	Name    string       `json:"name"`
	Help    string       `json:"help"`
	Type    string       `json:"type"`
	Metrics []jsonSample `json:"metrics"`
}

type jsonSample struct {
	Labels  map[string]string `json:"labels,omitempty"`
	Value   *float64          `json:"value,omitempty"`
	Count   *uint64           `json:"count,omitempty"`
	Sum     *float64          `json:"sum,omitempty"`
	Buckets []jsonBucket      `json:"buckets,omitempty"`
}

type jsonBucket struct {
	UpperBound float64 `json:"upper_bound"`
	Count      uint64  `json:"count"`
}

func metricFamiliesToJSON(mfs []*dto.MetricFamily) ([]byte, error) {
	out := make([]jsonMetricFamily, 0, len(mfs))
	for _, mf := range mfs {
		family := jsonMetricFamily{
			Name: mf.GetName(),
			Help: mf.GetHelp(),
			Type: mf.GetType().String(),
		}
		for _, metric := range mf.GetMetric() {
			family.Metrics = append(family.Metrics, metricToJSONSample(mf.GetType(), metric))
		}
		out = append(out, family)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal metrics json: %w", err)
	}
	return data, nil
}

func metricToJSONSample(metricType dto.MetricType, metric *dto.Metric) jsonSample {
	sample := jsonSample{Labels: labelMap(metric.GetLabel())}

	switch metricType {
	case dto.MetricType_COUNTER:
		value := metric.GetCounter().GetValue()
		sample.Value = &value
	case dto.MetricType_GAUGE:
		value := metric.GetGauge().GetValue()
		sample.Value = &value
	case dto.MetricType_HISTOGRAM:
		h := metric.GetHistogram()
		count := h.GetSampleCount()
		sum := h.GetSampleSum()
		sample.Count = &count
		sample.Sum = &sum
		for _, b := range h.GetBucket() {
			sample.Buckets = append(sample.Buckets, jsonBucket{
				UpperBound: b.GetUpperBound(),
				Count:      b.GetCumulativeCount(),
			})
		}
	case dto.MetricType_SUMMARY:
		s := metric.GetSummary()
		count := s.GetSampleCount()
		sum := s.GetSampleSum()
		sample.Count = &count
		sample.Sum = &sum
	}

	return sample
}

func labelMap(labels []*dto.LabelPair) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for _, lp := range labels {
		out[lp.GetName()] = lp.GetValue()
	}
	return out
}
