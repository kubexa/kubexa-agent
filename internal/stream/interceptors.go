package stream

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/protoversion"
)

// AgentVersion is the semver reported to the gateway; override at link time when needed.
var AgentVersion = "dev"

var sensitiveMetadataKeys = map[string]struct{}{
	"x-tenant-token": {},
	"authorization":  {},
	"tenant_token":   {},
}

type configProvider func() *config.Config

// interceptorDeps bundles dependencies for client interceptors.
type interceptorDeps struct {
	cfg    configProvider
	log    *logger.Logger
	metrics *grpcMetrics
}

func (d interceptorDeps) chainUnary() []grpc.UnaryClientInterceptor {
	return []grpc.UnaryClientInterceptor{
		d.recoveryUnary(),
		d.authUnary(),
		d.loggingUnary(),
		d.metricsUnary(),
	}
}

func (d interceptorDeps) chainStream() []grpc.StreamClientInterceptor {
	return []grpc.StreamClientInterceptor{
		d.recoveryStream(),
		d.authStream(),
		d.loggingStream(),
		d.metricsStream(),
	}
}

func (d interceptorDeps) recoveryUnary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("grpc unary panic on %s: %v", method, r)
				if d.log != nil {
					d.log.Error("gRPC unary panic recovered",
						logger.F("method", method),
						logger.F("panic", r),
						logger.F("stack", string(debug.Stack())),
					)
				}
			}
		}()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func (d interceptorDeps) recoveryStream() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (stream grpc.ClientStream, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("grpc stream panic on %s: %v", method, r)
				if d.log != nil {
					d.log.Error("gRPC stream panic recovered",
						logger.F("method", method),
						logger.F("panic", r),
						logger.F("stack", string(debug.Stack())),
					)
				}
			}
		}()
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func (d interceptorDeps) authUnary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = d.attachAuth(ctx)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func (d interceptorDeps) authStream() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = d.attachAuth(ctx)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func (d interceptorDeps) attachAuth(ctx context.Context) context.Context {
	cfg := d.cfg()
	if cfg == nil {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"x-tenant-token", cfg.Agent.TenantToken,
		"x-agent-version", AgentVersion,
		"x-proto-version", protoversion.Current,
		"x-cluster-id", cfg.Agent.ClusterID,
	)
}

func (d interceptorDeps) loggingUnary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		d.logRPCCompletion(method, time.Since(start), err, outgoingMetadataForLog(ctx))
		return err
	}
}

func (d interceptorDeps) loggingStream() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if d.log != nil {
			d.log.Debug("gRPC stream opening",
				logger.F("method", method),
				logger.F("metadata", redactMetadata(outgoingMetadataForLog(ctx))),
			)
		}
		stream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			d.logRPCCompletion(method, 0, err, outgoingMetadataForLog(ctx))
			return nil, err
		}
		return &loggingClientStream{
			ClientStream: stream,
			method:       method,
			log:          d.log,
			start:        time.Now(),
		}, nil
	}
}

type loggingClientStream struct {
	grpc.ClientStream
	method string
	log    *logger.Logger
	start  time.Time
}

func (s *loggingClientStream) CloseSend() error {
	err := s.ClientStream.CloseSend()
	s.logStreamClose(err)
	return err
}

func (s *loggingClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.logStreamClose(err)
	}
	return err
}

func (s *loggingClientStream) SendMsg(m any) error {
	err := s.ClientStream.SendMsg(m)
	if err != nil {
		s.logStreamClose(err)
	}
	return err
}

func (s *loggingClientStream) logStreamClose(err error) {
	if s.log == nil {
		return
	}
	fields := []logger.Field{
		logger.F("method", s.method),
		logger.F("duration", time.Since(s.start)),
	}
	if err != nil {
		s.log.Err(err).Warn("gRPC stream closed", fields...)
		return
	}
	s.log.Debug("gRPC stream closed", fields...)
}

func (d interceptorDeps) logRPCCompletion(method string, dur time.Duration, err error, md metadata.MD) {
	if d.log == nil {
		return
	}
	code := status.Code(err)
	fields := []logger.Field{
		logger.F("method", method),
		logger.F("duration", dur),
		logger.F("code", code.String()),
		logger.F("metadata", redactMetadata(md)),
	}
	switch {
	case err == nil:
		d.log.Debug("gRPC call completed", fields...)
	case isRetryableCode(code):
		d.log.Err(err).Warn("gRPC call failed (retryable)", fields...)
	default:
		d.log.Err(err).Error("gRPC call failed", fields...)
	}
}

func isRetryableCode(code codes.Code) bool {
	switch code { //nolint:exhaustive
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	default:
		return false
	}
}

func (d interceptorDeps) metricsUnary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		if d.metrics != nil {
			st := "ok"
			if err != nil {
				st = status.Code(err).String()
			}
			d.metrics.requestsTotal.WithLabelValues(method, st).Inc()
			d.metrics.requestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
		}
		return err
	}
}

func (d interceptorDeps) metricsStream() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		stream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			if d.metrics != nil {
				d.metrics.streamErrorsTotal.WithLabelValues("open").Inc()
			}
			return nil, err
		}
		if d.metrics != nil {
			d.metrics.setStreamActive(true)
		}
		return &metricsClientStream{
			ClientStream: stream,
			metrics:      d.metrics,
		}, nil
	}
}

type metricsClientStream struct {
	grpc.ClientStream
	metrics *grpcMetrics
	closed  bool
}

func (s *metricsClientStream) SendMsg(m any) error {
	err := s.ClientStream.SendMsg(m)
	if err == nil && s.metrics != nil {
		s.metrics.streamMessagesSent.Inc()
	} else if err != nil && s.metrics != nil {
		s.metrics.streamErrorsTotal.WithLabelValues("send").Inc()
	}
	return err
}

func (s *metricsClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err == nil && s.metrics != nil {
		s.metrics.streamMessagesReceived.Inc()
	} else if err != nil && s.metrics != nil {
		s.metrics.streamErrorsTotal.WithLabelValues("recv").Inc()
	}
	return err
}

func (s *metricsClientStream) CloseSend() error {
	err := s.ClientStream.CloseSend()
	s.markClosed()
	return err
}

func (s *metricsClientStream) markClosed() {
	if s.closed || s.metrics == nil {
		return
	}
	s.closed = true
	s.metrics.setStreamActive(false)
}

func outgoingMetadataForLog(ctx context.Context) metadata.MD {
	md, _ := metadata.FromOutgoingContext(ctx)
	return md
}

func redactMetadata(md metadata.MD) metadata.MD {
	if len(md) == 0 {
		return md
	}
	out := metadata.MD{}
	for k, vals := range md {
		key := strings.ToLower(k)
		if _, sensitive := sensitiveMetadataKeys[key]; sensitive {
			cp := make([]string, len(vals))
			for i := range vals {
				cp[i] = "***"
			}
			out[k] = cp
			continue
		}
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}
