// Package pprof serves Go profiles on a dedicated, opt-in listener.
//
// It is OFF unless an address is configured, and the address it is meant to
// carry is a loopback one. Both choices are about what a heap dump contains:
// this agent's heap holds unredacted Secret values from the state watcher's
// informer cache and raw log lines from every collected pod. A profile endpoint
// is therefore an exfiltration path for exactly the data the agent exists to
// handle carefully, and it must not be reachable from the cluster network.
//
// Binding 127.0.0.1 is what makes that true while keeping the endpoint usable:
// `kubectl port-forward` attaches to the pod's own network namespace, so an
// operator holding pods/portforward RBAC still reaches it and nothing else on
// the network does. For the same reason the chart neither publishes this port
// on the Service nor declares it as a containerPort -- both would advertise an
// exposure that is not intended.
package pprof

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	httppprof "net/http/pprof"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

const shutdownTimeout = 5 * time.Second

// Server serves /debug/pprof on its own listener.
type Server struct {
	addr    string
	log     *logger.Logger
	httpSrv *http.Server
}

// NewServer returns a profile server bound to addr, or nil when addr is empty.
//
// A nil Server is the disabled case and every method tolerates it, so callers
// wire it up unconditionally and the "off" path costs one nil check rather than
// a branch at every use.
func NewServer(addr string, log *logger.Logger) *Server {
	if addr == "" {
		return nil
	}
	if log == nil {
		log = logger.New("pprof")
	}

	// Handlers are registered by hand on a private mux. Note what this does NOT
	// buy: importing net/http/pprof registers the same handlers on
	// http.DefaultServeMux unconditionally, at import time, and no amount of
	// private wiring undoes that. So the invariant that actually keeps profiles
	// off the network is "no server in this binary serves DefaultServeMux" --
	// i.e. no nil Handler and no ListenAndServe(addr, nil) anywhere. A test in
	// this package scans the source for both shapes, because that is the way the
	// property gets broken later and nothing else would notice.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)

	return &Server{
		addr: addr,
		log:  log,
		httpSrv: &http.Server{
			Addr:    addr,
			Handler: mux,
			// No write timeout: a CPU profile blocks for its whole duration
			// (30s by default) and a timeout here would truncate every one.
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// Addr reports the configured listen address, or "" when disabled.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Run serves until ctx is cancelled, then shuts down gracefully. A nil Server
// returns immediately: profiling is off and that is not an error.
func (s *Server) Run(ctx context.Context) error {
	if s == nil {
		return nil
	}

	s.log.Info("pprof server listening", logger.F("addr", s.addr))

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
	case err, ok := <-errCh:
		if ok && err != nil {
			return fmt.Errorf("pprof server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("pprof server shutdown: %w", err)
	}
	s.log.Info("pprof server stopped")
	return nil
}
