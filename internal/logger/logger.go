// Package logger provides a context-aware structured logger built on zerolog.
package logger

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// Level represents a log severity.
type Level int8

const (
	// LevelDebug is the most verbose log level.
	LevelDebug Level = iota
	// LevelInfo is the default operational log level.
	LevelInfo
	// LevelWarn indicates a potentially harmful situation.
	LevelWarn
	// LevelError indicates a failure that should be investigated.
	LevelError
)

// String returns the canonical level name.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "unknown"
	}
}

// ParseLevel parses a level name (case-insensitive).
func ParseLevel(s string) (Level, error) {
	switch s {
	case "debug", "DEBUG":
		return LevelDebug, nil
	case "info", "INFO":
		return LevelInfo, nil
	case "warn", "warning", "WARN", "WARNING":
		return LevelWarn, nil
	case "error", "ERROR":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("logger: unknown level %q", s)
	}
}

var runtimeLevel atomic.Int32

func init() {
	runtimeLevel.Store(int32(LevelInfo))
}

// SetLevel updates the global minimum log level at runtime without restart.
func SetLevel(level Level) {
	runtimeLevel.Store(int32(level))
}

// GetLevel returns the current global minimum log level.
func GetLevel() Level {
	return Level(runtimeLevel.Load())
}

// Field is a key-value pair attached to a log entry.
type Field struct {
	key   string
	value any
}

// F constructs a Field from a key and value.
func F(key string, value any) Field {
	return Field{key: key, value: value}
}

// Option configures logger construction.
type Option func(*options)

type options struct {
	clusterID   string
	agentID     string
	development bool
	level       Level
	levelSet    bool
	writer      io.Writer
}

// WithClusterID sets the cluster_id field on every log entry.
func WithClusterID(id string) Option {
	return func(o *options) {
		o.clusterID = id
	}
}

// WithAgentID sets the agent_id field on every log entry.
func WithAgentID(id string) Option {
	return func(o *options) {
		o.agentID = id
	}
}

// WithDevelopment enables human-readable console output instead of JSON.
func WithDevelopment(enabled bool) Option {
	return func(o *options) {
		o.development = enabled
	}
}

// WithLevel sets the global minimum log level when the logger is constructed.
func WithLevel(level Level) Option {
	return func(o *options) {
		o.level = level
		o.levelSet = true
	}
}

// WithWriter overrides the default log output destination (stdout).
func WithWriter(w io.Writer) Option {
	return func(o *options) {
		o.writer = w
	}
}

// Logger is a structured, context-aware logger for a single component.
type Logger struct {
	zlog      zerolog.Logger
	component string
	noop      bool

	pendingErr error
	withStack  bool
}

// New creates a component-scoped logger. Each component should call New once.
func New(component string, opts ...Option) *Logger {
	cfg := options{}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.levelSet {
		SetLevel(cfg.level)
	}

	w := cfg.writer
	if w == nil {
		w = os.Stdout
	}
	if cfg.development {
		w = zerolog.ConsoleWriter{Out: w, TimeFormat: "15:04:05"}
	}

	z := zerolog.New(w).
		With().
		Timestamp().
		Str("component", component).
		Str("cluster_id", cfg.clusterID).
		Str("agent_id", cfg.agentID).
		Logger()

	return &Logger{
		zlog:      z,
		component: component,
	}
}

// With returns a child logger with an additional permanent field.
func (l *Logger) With(key string, value any) *Logger {
	if l == nil || l.noop {
		return l
	}
	child := l.clone()
	child.zlog = child.zlog.With().Interface(key, value).Logger()
	return child
}

// WithContext returns a child logger enriched with trace fields from ctx.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	if l == nil || l.noop {
		return l
	}
	if ctx == nil {
		return l
	}

	child := l.clone()
	evt := child.zlog.With()
	added := false

	if traceID, ok := traceIDFromContext(ctx); ok {
		evt = evt.Str("trace_id", traceID)
		added = true
	}
	if spanID, ok := spanIDFromContext(ctx); ok {
		evt = evt.Str("span_id", spanID)
		added = true
	}
	if requestID, ok := requestIDFromContext(ctx); ok {
		evt = evt.Str("request_id", requestID)
		added = true
	}

	if added {
		child.zlog = evt.Logger()
	}
	return child
}

// Err attaches an error to the next log line. When debug level is enabled,
// the error is recorded with a stack trace.
func (l *Logger) Err(err error) *Logger {
	if l == nil || l.noop || err == nil {
		return l
	}
	child := l.clone()
	child.pendingErr = err
	child.withStack = l.enabled(LevelDebug)
	return child
}

// Debug logs at debug level.
func (l *Logger) Debug(msg string, fields ...Field) {
	l.log(LevelDebug, msg, fields...)
}

// Info logs at info level.
func (l *Logger) Info(msg string, fields ...Field) {
	l.log(LevelInfo, msg, fields...)
}

// Warn logs at warn level.
func (l *Logger) Warn(msg string, fields ...Field) {
	l.log(LevelWarn, msg, fields...)
}

// Error logs at error level.
func (l *Logger) Error(msg string, fields ...Field) {
	l.log(LevelError, msg, fields...)
}

func (l *Logger) log(level Level, msg string, fields ...Field) {
	if l == nil || l.noop || !l.enabled(level) {
		return
	}

	var evt *zerolog.Event
	switch level {
	case LevelDebug:
		evt = l.zlog.Debug()
	case LevelInfo:
		evt = l.zlog.Info()
	case LevelWarn:
		evt = l.zlog.Warn()
	case LevelError:
		evt = l.zlog.Error()
	default:
		evt = l.zlog.Info()
	}

	if l.pendingErr != nil {
		evt = evt.Err(l.pendingErr)
		if l.withStack {
			evt = evt.Str("stack", string(debug.Stack()))
		}
	}

	for _, f := range fields {
		evt = evt.Interface(f.key, f.value)
	}

	evt.Msg(msg)
}

func (l *Logger) enabled(level Level) bool {
	return level >= Level(runtimeLevel.Load())
}

func (l *Logger) clone() *Logger {
	if l == nil {
		return noopLogger
	}
	dup := *l
	return &dup
}

// noopLogger discards all log output.
var noopLogger = &Logger{noop: true}

// FromContext returns the logger stored in ctx, or a no-op logger if absent.
func FromContext(ctx context.Context) *Logger {
	if ctx == nil {
		return noopLogger
	}
	if l, ok := ctx.Value(loggerKey{}).(*Logger); ok && l != nil {
		return l
	}
	return noopLogger
}

// NewContext stores l in ctx for retrieval via FromContext.
func NewContext(ctx context.Context, l *Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loggerKey{}, l)
}

type loggerKey struct{}

type traceKey struct{}

// WithTraceID stores a trace ID in ctx for WithContext extraction.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceKey{}, traceID)
}

// WithSpanID stores a span ID in ctx for WithContext extraction.
func WithSpanID(ctx context.Context, spanID string) context.Context {
	return context.WithValue(ctx, spanKey{}, spanID)
}

// WithRequestID stores a request ID in ctx for WithContext extraction.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestKey{}, requestID)
}

type spanKey struct{}
type requestKey struct{}

func traceIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(traceKey{}).(string)
	return v, ok && v != ""
}

func spanIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(spanKey{}).(string)
	return v, ok && v != ""
}

func requestIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(requestKey{}).(string)
	return v, ok && v != ""
}

// Interface defines structured logging operations for test doubles.
type Interface interface {
	With(key string, value any) *Logger
	WithContext(ctx context.Context) *Logger
	Err(err error) *Logger
	Debug(msg string, fields ...Field)
	Info(msg string, fields ...Field)
	Warn(msg string, fields ...Field)
	Error(msg string, fields ...Field)
}

// Ensure Logger implements Interface.
var _ Interface = (*Logger)(nil)
