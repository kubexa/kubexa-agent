// Package health exposes HTTP liveness and readiness probes for kubexa-agent.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/metrics"
)

const shutdownTimeout = 5 * time.Second

// HealthChecker performs a component-level readiness probe.
type HealthChecker interface {
	// Name returns the component name (e.g. "k8s", "gateway", "queue").
	Name() string

	// Check performs a health check and returns nil if healthy.
	Check(ctx context.Context) error
}

// HealthConfig configures the health HTTP server.
type HealthConfig struct {
	Addr          string
	LivenessPath  string
	ReadinessPath string
	Timeout       time.Duration
}

// DefaultConfig returns HealthConfig populated with production defaults.
func DefaultConfig() HealthConfig {
	return HealthConfig{
		Addr:          ":8080",
		LivenessPath:  "/healthz",
		ReadinessPath: "/readyz",
		Timeout:       5 * time.Second,
	}
}

func (c HealthConfig) withDefaults() HealthConfig {
	def := DefaultConfig()
	if c.Addr == "" {
		c.Addr = def.Addr
	}
	if c.LivenessPath == "" {
		c.LivenessPath = def.LivenessPath
	}
	if c.ReadinessPath == "" {
		c.ReadinessPath = def.ReadinessPath
	}
	if c.Timeout <= 0 {
		c.Timeout = def.Timeout
	}
	return c
}

// Server serves Kubernetes-style health probes and component readiness state.
type Server struct {
	cfg     HealthConfig
	log     *logger.Logger
	metrics *metrics.HealthMetrics

	mu        sync.RWMutex
	checkers  map[string]HealthChecker
	lastState map[string]bool

	httpSrv *http.Server
	ln      net.Listener
}

// New constructs a health Server bound to cfg.
func New(cfg HealthConfig, log *logger.Logger, metrics *metrics.HealthMetrics) *Server {
	cfg = cfg.withDefaults()
	if log == nil {
		log = logger.New("health")
	}

	s := &Server{
		cfg:       cfg,
		log:       log,
		metrics:   metrics,
		checkers:  make(map[string]HealthChecker),
		lastState: make(map[string]bool),
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.LivenessPath, http.HandlerFunc(s.handleLiveness))
	mux.Handle(cfg.ReadinessPath, http.HandlerFunc(s.handleReadiness))

	s.httpSrv = &http.Server{
		Handler: mux,
	}
	return s
}

// Register adds or replaces a component health checker by name.
func (s *Server) Register(checker HealthChecker) {
	if s == nil || checker == nil {
		return
	}
	name := checker.Name()
	if name == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkers[name] = checker
}

// Start binds the HTTP listener and serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	if s == nil || s.httpSrv == nil {
		return fmt.Errorf("health server: not initialized")
	}

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("health server listen on %s: %w", s.cfg.Addr, err)
	}
	s.ln = ln
	s.httpSrv.Addr = ln.Addr().String()

	s.log.Info("health server listening", logger.F("addr", s.httpSrv.Addr))

	errCh := make(chan error, 1)
	go func() {
		err := s.httpSrv.Serve(ln)
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
			return fmt.Errorf("health server shutdown: %w", err)
		}
		if err := <-errCh; err != nil {
			return fmt.Errorf("health server: %w", err)
		}
		return ctx.Err()
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("health server: %w", err)
		}
		return nil
	}
}

type livenessResponse struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
}

type readinessResponse struct {
	Status     string                       `json:"status"`
	Timestamp  time.Time                    `json:"timestamp"`
	Components map[string]componentResult `json:"components,omitempty"`
}

type componentResult struct {
	Status string  `json:"status"`
	Error  *string `json:"error"`
}

func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeMethodNotAllowed(w, r)
		return
	}

	start := time.Now()
	requestID := s.setResponseHeaders(w)

	body := livenessResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
	}
	s.writeJSON(w, r, requestID, start, http.StatusOK, body)
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeMethodNotAllowed(w, r)
		return
	}

	start := time.Now()
	requestID := s.setResponseHeaders(w)
	verbose := r.URL.Query().Get("verbose") == "true"

	checkers := s.snapshotCheckers()
	components := s.runChecks(r.Context(), checkers)

	allHealthy := true
	for _, result := range components {
		if result.Status != "ok" {
			allHealthy = false
			break
		}
	}

	overall := "ok"
	statusCode := http.StatusOK
	if !allHealthy {
		overall = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	s.updateMetricsAndLogTransitions(components)

	resp := readinessResponse{
		Status:    overall,
		Timestamp: time.Now().UTC(),
	}
	if verbose || !allHealthy {
		resp.Components = components
	}

	s.writeJSON(w, r, requestID, start, statusCode, resp)
}

func (s *Server) snapshotCheckers() map[string]HealthChecker {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]HealthChecker, len(s.checkers))
	for name, checker := range s.checkers {
		out[name] = checker
	}
	return out
}

func (s *Server) runChecks(parent context.Context, checkers map[string]HealthChecker) map[string]componentResult {
	results := make(map[string]componentResult, len(checkers))
	if len(checkers) == 0 {
		return results
	}

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)

	for name, checker := range checkers {
		wg.Add(1)
		go func(name string, checker HealthChecker) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
			defer cancel()

			err := checker.Check(ctx)

			status, msg := componentStatus(err)
			mu.Lock()
			results[name] = componentResult{
				Status: status,
				Error:  msg,
			}
			mu.Unlock()
		}(name, checker)
	}

	wg.Wait()
	return results
}

func (s *Server) updateMetricsAndLogTransitions(components map[string]componentResult) {
	for name, result := range components {
		healthy := result.Status == "ok"

		if s.metrics != nil {
			if healthy {
				s.metrics.SetHealthy(name)
			} else {
				s.metrics.SetUnhealthy(name)
			}
		}

		s.mu.Lock()
		prev, seen := s.lastState[name]
		s.lastState[name] = healthy
		s.mu.Unlock()

		if seen && prev == healthy {
			continue
		}
		if !seen && healthy {
			continue
		}

		errMsg := ""
		if result.Error != nil {
			errMsg = *result.Error
		}

		if healthy {
			s.log.Info("component health recovered",
				logger.F("component", name),
				logger.F("status", result.Status),
			)
			continue
		}

		s.log.Warn("component health degraded",
			logger.F("component", name),
			logger.F("status", result.Status),
			logger.F("error", errMsg),
		)
	}
}

func (s *Server) setResponseHeaders(w http.ResponseWriter) string {
	requestID := uuid.NewString()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	return requestID
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, requestID string, start time.Time, status int, payload any) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.log.Warn("failed to encode health response", logger.F("error", err))
	}

	s.log.Debug("health request",
		logger.F("method", r.Method),
		logger.F("path", r.URL.Path),
		logger.F("duration", time.Since(start)),
		logger.F("status", status),
		logger.F("request_id", requestID),
	)
}

func (s *Server) writeMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := s.setResponseHeaders(w)
	w.WriteHeader(http.StatusMethodNotAllowed)
	s.log.Debug("health request",
		logger.F("method", r.Method),
		logger.F("path", r.URL.Path),
		logger.F("duration", time.Since(start)),
		logger.F("status", http.StatusMethodNotAllowed),
		logger.F("request_id", requestID),
	)
}
