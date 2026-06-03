package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const (
	streamBackoffInitial = time.Second
	streamBackoffMax     = 30 * time.Second
	streamErrorWindow    = 5 * time.Minute
	streamErrorThreshold = 10
	streamCircuitPause   = 5 * time.Minute
	// reconnectSinceOverlap rewinds the resume point slightly to avoid gaps when
	// kubelet timestamps and parsed line timestamps disagree.
	reconnectSinceOverlap = 5 * time.Second
)

type streamKey struct {
	podUID        string
	containerName string
}

func (k streamKey) String() string {
	return k.podUID + "/" + k.containerName
}

// streamTarget identifies a single pod container log source.
type streamTarget struct {
	key       streamKey
	pod       *corev1.Pod
	container string
	rule      ruleView
}

type ruleView struct {
	id         string
	ruleIDs    []string
	tailLines  int64
	follow     bool
	labelSel   string
	fieldSel   string
	podNames   []string
	containers []string
}

// streamHandle tracks an active log stream goroutine.
type streamHandle struct {
	cancel   context.CancelFunc
	starting bool
}

type errorBudget struct {
	mu       sync.Mutex
	errors   []time.Time
	circuit  time.Time
}

func (b *errorBudget) record(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.circuit.IsZero() && now.Before(b.circuit) {
		return false
	}
	if !b.circuit.IsZero() && !now.Before(b.circuit) {
		b.circuit = time.Time{}
		b.errors = b.errors[:0]
	}

	cutoff := now.Add(-streamErrorWindow)
	kept := b.errors[:0]
	for _, t := range b.errors {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	b.errors = kept
	if len(b.errors) > streamErrorThreshold {
		b.circuit = now.Add(streamCircuitPause)
		b.errors = b.errors[:0]
		return false
	}
	return true
}

func (b *errorBudget) inCircuit(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.circuit.IsZero() && now.Before(b.circuit)
}

func streamBackoff(attempt int, rng *rand.Rand) time.Duration {
	delay := streamBackoffInitial
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= streamBackoffMax {
			delay = streamBackoffMax
			break
		}
	}
	if delay > streamBackoffMax {
		delay = streamBackoffMax
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	}
	jitter := 0.8 + rng.Float64()*0.4
	return time.Duration(float64(delay) * jitter)
}

func (c *Collector) runStream(parent context.Context, target streamTarget) {
	streamCtx, cancel := context.WithCancel(parent)

	c.streamsMu.Lock()
	if existing, ok := c.streams[target.key]; ok && existing.starting {
		existing.cancel = cancel
		existing.starting = false
	} else {
		if existing != nil {
			existing.cancel()
		}
		c.streams[target.key] = &streamHandle{cancel: cancel}
	}
	c.streamsMu.Unlock()

	defer func() {
		cancel()
		c.releaseStream(target.key)
		c.sem.Release(1)
		c.updateActiveStreamsMetric()
	}()

	log := c.log.With("namespace", target.pod.Namespace).
		With("pod", target.pod.Name).
		With("container", target.container).
		With("pod_uid", string(target.pod.UID))

	budget := &errorBudget{}
	cursor := newStreamCursor(
		streamCtx,
		c.checkpoints,
		target.key.checkpointKey(),
		checkpoint.Metadata{Namespace: target.pod.Namespace, PodName: target.pod.Name},
		c.metrics,
	)
	defer cursor.close()

	if since, ok := cursor.sinceForReconnect(reconnectSinceOverlap); ok {
		log.Debug("loaded log stream checkpoint",
			logger.F("since_time", since.Format(time.RFC3339Nano)),
			logger.F("overlap", reconnectSinceOverlap.String()),
		)
	}

	attempt := 0
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec

	for {
		if err := streamCtx.Err(); err != nil {
			return
		}

		now := time.Now().UTC()
		if budget.inCircuit(now) {
			if err := c.sleep(streamCtx, streamCircuitPause); err != nil {
				return
			}
			continue
		}

		err := c.consumeStream(streamCtx, log, target, cursor)
		if err == nil {
			if !target.rule.follow {
				return
			}
			attempt = 0
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		c.metrics.incStreamError(classifyStreamError(err))
		if !budget.record(now) {
			log.Warn("log stream error budget exceeded; pausing stream",
				logger.F("errors", streamErrorThreshold),
				logger.F("window", streamErrorWindow.String()),
				logger.F("pause", streamCircuitPause.String()),
			)
			if err := c.sleep(streamCtx, streamCircuitPause); err != nil {
				return
			}
			continue
		}

		log.Warn("log stream ended; retrying", logger.F("error", err.Error()), logger.F("attempt", attempt))
		delay := streamBackoff(attempt, rng)
		attempt++
		if err := c.sleep(streamCtx, delay); err != nil {
			return
		}
	}
}

func classifyStreamError(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, io.EOF) {
		return "eof"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "io"
}

func (c *Collector) consumeStream(ctx context.Context, log *logger.Logger, target streamTarget, cursor *streamCursor) error {
	opts := k8s.LogOptions{
		Follow:     target.rule.follow,
		Timestamps: true,
	}
	if since, ok := cursor.sinceForReconnect(reconnectSinceOverlap); ok {
		opts.SinceTime = since
		log.Debug("resuming log stream",
			logger.F("since_time", since.Format(time.RFC3339Nano)),
			logger.F("overlap", reconnectSinceOverlap.String()),
		)
	} else {
		opts.TailLines = target.rule.tailLines
	}

	reader, err := c.kube.Logs(ctx, target.pod.Namespace, target.pod.Name, target.container, opts)
	if err != nil {
		return fmt.Errorf("open log stream: %w", err)
	}
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	const maxLineSize = 256 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		c.metrics.addBytes(len(line))
		parsed := ParseLine(line, time.Now().UTC())
		if parsed.Message == "" && len(parsed.Raw) == 0 {
			continue
		}
		if err := c.handleLogLine(ctx, target, parsed); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			log.Warn("failed to handle log line", logger.F("error", err.Error()))
			continue
		}
		cursor.markProcessed(parsed.Timestamp)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if target.rule.follow {
		return io.EOF
	}
	return nil
}

func (c *Collector) handleLogLine(ctx context.Context, target streamTarget, parsed ParsedLine) error {
	entry := &agentv1.LogEntry{
		PodName:   target.pod.Name,
		Namespace: target.pod.Namespace,
		Container: target.container,
		Timestamp: parsed.Timestamp.UnixNano(),
		Message:   parsed.Message,
		Level:     parsed.Level,
		Labels:    FilterPodLabels(target.pod.Labels),
		Raw:       parsed.Raw,
	}

	msg := &agentv1.AgentMessage{
		MessageId: uuid.NewString(),
		Meta:      c.metaSnapshot(parsed.Timestamp),
		Payload: &agentv1.AgentMessage_Logs{
			Logs: &agentv1.LogBatch{Entries: []*agentv1.LogEntry{entry}},
		},
	}

	if err := c.writer.Write(ctx, msg); err != nil {
		c.metrics.incDropped(target.pod.Namespace, target.pod.Name, dropReason(err))
		return err
	}

	c.metrics.incLines(target.pod.Namespace, target.pod.Name, LevelLabel(parsed.Level))
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
