package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
	"github.com/kubexa/kubexa-agent/internal/ingestrules"
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

// A joined record is capped well under the 256KB line cap so a runaway trace
// cannot grow one record without bound, and is held at most this long so the
// last line of a stream that went quiet still reaches the queue.
//
// flushJoiner's detached write on teardown is bounded by Collector.writeTimeout
// (the same per-write budget queueWriter enforces) rather than a constant
// here, so it stays consistent with the collector's actual configured write
// timeout instead of a guessed duration.
const (
	maxJoinedBytes = 64 * 1024
	maxJoinHold    = 2 * time.Second
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
	mu      sync.Mutex
	errors  []time.Time
	circuit time.Time
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

		delivered, err := c.consumeStream(streamCtx, log, target, cursor)
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

		attempt = attemptAfterRead(attempt, delivered)
		log.Warn("log stream ended; retrying",
			logger.F("error", err.Error()),
			logger.F("attempt", attempt),
			logger.F("delivered", delivered),
		)
		delay := streamBackoff(attempt, rng)
		attempt++
		if err := c.sleep(streamCtx, delay); err != nil {
			return
		}
	}
}

// resumeSince decides where a reconnecting stream actually resumes.
//
// The checkpoint alone is wrong for a stream that has been quiet for longer
// than max_sample_age. kubelet closes an idle log stream at its
// streamingConnectionIdleTimeout -- 4h by default -- and every stream opened at
// agent start goes idle together, so the quiet ones reconnect as one
// synchronized cohort on that clock. sinceForReconnect then asks the API for
// logs since the last line it processed, and for a pod silent longer than
// max_sample_age that is a timestamp the drain will certainly reject: the agent
// re-reads a line it delivered a week ago, enqueues it, drops it at
// ageDropReason, and its heartbeat reports the drop to the platform as data
// loss. Once per quiet stream, every four hours, indefinitely.
//
// Clamping to the age floor cannot cost anything that could have been ingested,
// because a line older than the floor is one ageDropReason would discard
// anyway. It also does not suppress the condition the drop counter exists for:
// records that expire while sitting in the WAL still age out there and still
// count. Only the self-inflicted re-read goes away.
//
// A zero max_sample_age means no rule was pushed, and then nothing about age
// may change behaviour -- the same contract the age filter itself keeps.
func resumeSince(checkpoint time.Time, maxSampleAge time.Duration, now time.Time) time.Time {
	if maxSampleAge <= 0 {
		return checkpoint
	}
	// ageDropReason drops on strictly greater, so a line exactly at the floor
	// is still deliverable and the floor itself is a safe resume point.
	if floor := now.Add(-maxSampleAge); checkpoint.Before(floor) {
		return floor
	}
	return checkpoint
}

// logReadPlan is what to ask the Kubernetes log API for on one container
// stream.
type logReadPlan struct {
	opts k8s.LogOptions
	// clampedFrom is the checkpoint that was refused for being older than the
	// age floor, and is zero when nothing was clamped. It exists only so the
	// caller can log what it would have resumed from.
	clampedFrom time.Time
}

// planLogRead decides the read for one stream. It is a function rather than a
// dozen lines inside consumeStream because its two branches have to AGREE
// about the age floor, and until 2026-11-08 they did not.
//
// The reconnect branch was floored first, for the reason resumeSince above
// records. The fresh-start branch was not, and it is reached on every agent
// start, because streamCursor is in-memory only: the pod persists the queue it
// has not delivered yet on a PVC and deliberately forgets where it was
// reading. So a restart asks for each container's last tailLines lines with no
// lower bound on their age, and for a pod that has been quiet longer than
// max_sample_age every one of those lines is older than the floor -- read,
// enqueued, discarded at ageDropReason, and reported to the platform on the
// heartbeat as data loss. Measured on apexcrawl 2026-11-08: five such drops
// within three minutes of a restart, on a cluster whose tailed pods are mostly
// busy.
//
// The same two arguments the reconnect clamp rests on apply unchanged. It
// cannot cost an ingestable line, because a line older than the floor is one
// the age filter would discard anyway. And it does not hide what the counter
// exists for: records that expire while waiting in the WAL still age out there
// and still count. What goes away is a loss figure for records that were never
// deliverable and were never ours.
//
// TailLines is kept alongside it. The Kubernetes API applies both bounds, so
// the read is "the last N lines, none older than the floor" -- and on a first
// install, where nothing has been delivered yet, that is the same read it
// always was minus the part that could only be thrown away.
//
// A zero max_sample_age means no rule has been pushed, and then nothing about
// age may change behaviour: the same contract resumeSince and ageDropReason
// both keep.
func planLogRead(
	target streamTarget,
	cursor *streamCursor,
	maxSampleAge time.Duration,
	now time.Time,
) logReadPlan {
	plan := logReadPlan{opts: k8s.LogOptions{
		Follow:     target.rule.follow,
		Timestamps: true,
	}}
	if since, ok := cursor.sinceForReconnect(reconnectSinceOverlap); ok {
		if clamped := resumeSince(since, maxSampleAge, now); !clamped.Equal(since) {
			plan.clampedFrom = since
			since = clamped
		}
		plan.opts.SinceTime = since
		return plan
	}
	plan.opts.TailLines = target.rule.tailLines
	if maxSampleAge > 0 {
		plan.opts.Since = maxSampleAge
	}
	return plan
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

// attemptAfterRead decides whether the read that just ended counts against
// the backoff. The caller still increments for the failure itself; this only
// says where that count starts from.
//
// A follow stream ALWAYS ends in an error -- io.EOF, by construction, every
// time kubelet closes it for being idle. So "did it return an error" cannot
// tell a healthy stream from a broken one, and the counter only ever climbed:
// apexcrawl's agent was reading attempt=16 after 35 hours, meaning every
// reconnect waited the full streamBackoffMax whether or not the previous read
// had worked perfectly. What does tell them apart is whether anything was
// read.
func attemptAfterRead(attempt, delivered int) int {
	if delivered > 0 {
		return 0
	}
	return attempt
}

// consumeStream reads one stream to its end and reports how many records it
// delivered, which is the only signal that separates a stream working
// normally from one failing to open at all.
func (c *Collector) consumeStream(ctx context.Context, log *logger.Logger, target streamTarget, cursor *streamCursor) (int, error) {
	plan := planLogRead(target, cursor, c.rules.Get().MaxSampleAge, time.Now())
	opts := plan.opts
	if !plan.clampedFrom.IsZero() {
		log.Debug("resume point clamped to the age floor",
			logger.F("checkpoint", plan.clampedFrom.Format(time.RFC3339Nano)),
			logger.F("resume_from", opts.SinceTime.Format(time.RFC3339Nano)),
		)
	}
	if !opts.SinceTime.IsZero() {
		log.Debug("resuming log stream",
			logger.F("since_time", opts.SinceTime.Format(time.RFC3339Nano)),
			logger.F("overlap", reconnectSinceOverlap.String()),
		)
	}

	delivered := 0

	reader, err := c.kube.Logs(ctx, target.pod.Namespace, target.pod.Name, target.container, opts)
	if err != nil {
		return delivered, fmt.Errorf("open log stream: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// The line cap is read once per stream rather than per line: a rule change
	// mid-stream takes effect on the next reconnect, and the authoritative cut
	// in handleLogLine reads the current rules on every record anyway.
	//
	// The reader's cap carries headroom for the Kubernetes log API's own
	// timestamp prefix, which ParseLine strips before anything reaches Loki.
	// Capping the transport line at exactly max_line_bytes would cut a payload
	// that is itself within the limit, and mark it truncated for bytes the
	// limit was never about.
	lr := newLineReader(reader, readerCap(c.rules.Get().MaxLineBytes))

	// One joiner per container stream. Lines from two containers interleave in
	// time, so a shared joiner would attach one program's frames to another's
	// panic.
	join := newJoiner(maxJoinedBytes, maxJoinHold)
	defer c.flushJoiner(ctx, log, target, join, cursor)

	for {
		if err := ctx.Err(); err != nil {
			return delivered, err
		}
		raw, truncated, err := lr.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return delivered, err
		}
		line := string(raw)
		c.metrics.addBytes(len(line))
		parsed := ParseLine(line, time.Now().UTC())
		parsed.Truncated = truncated
		if parsed.Message == "" && len(parsed.Raw) == 0 {
			continue
		}
		for _, record := range join.Add(parsed, time.Now()) {
			// A resume rewinds past the last line it handled, so the start of
			// every reconnected stream is lines this one has already sent.
			// Delivering them again is not free: it stores the line twice for
			// whoever reads the logs, and once the stream's head moves far
			// enough ahead Loki refuses the copy outright and the whole batch
			// fails with it.
			if cursor.alreadyDelivered(record) {
				c.metrics.incReplayedSkipped(target.pod.Namespace, target.pod.Name)
				continue
			}
			if err := c.handleLogLine(ctx, target, record); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return delivered, err
				}
				log.Warn("failed to handle log line", logger.F("error", err.Error()))
				continue
			}
			cursor.markDelivered(record)
			delivered++
		}
	}
	if target.rule.follow {
		return delivered, io.EOF
	}
	return delivered, nil
}

// flushJoiner writes out any record still held by the joiner when a stream
// ends, on a context that outlives the stream's own -- and tells the cursor
// what it delivered.
//
// The detached context is why this exists at all: a stream tears down on pod
// deletion, rule removal and agent shutdown as well as on reconnect, and on a
// genuine teardown the record inside the joiner's hold window -- often the
// tail of a multi-line stack trace -- never gets a second chance to be sent.
// The replacement context is bounded by writeTimeout so a stuck downstream
// queue still cannot hold shutdown open.
//
// Marking the cursor is the other half, and it was missing. On a quiet stream
// the record flushed here is the LAST line, every time: the joiner holds a
// record until the next line arrives, and on a pod that writes once a day the
// next line is a day away. While this advanced nothing, the cursor sat
// permanently one record behind the stream, so every reconnect resumed from
// before a line that had already been sent, and sent it again. Measured on
// apexcrawl 2026-11-11: one cert-manager-webhook line from 11-08 stored seven
// times and still being re-sent three days later.
func (c *Collector) flushJoiner(streamCtx context.Context, log *logger.Logger, target streamTarget, join *joiner, cursor *streamCursor) {
	records := join.Flush()
	if len(records) == 0 {
		return
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), c.writeTimeout)
	defer cancel()
	for _, record := range records {
		if cursor.alreadyDelivered(record) {
			c.metrics.incReplayedSkipped(target.pod.Namespace, target.pod.Name)
			continue
		}
		if err := c.handleLogLine(flushCtx, target, record); err != nil {
			log.Warn("failed to handle log line", logger.F("error", err.Error()))
			continue
		}
		cursor.markDelivered(record)
	}
}

// buildLogEntry is the single place a parsed line becomes a wire entry, so the
// fields the consumer indexes cannot drift from the fields the parser fills.
func buildLogEntry(target streamTarget, parsed ParsedLine, workload workloadRef) *agentv1.LogEntry {
	return &agentv1.LogEntry{
		PodName:      target.pod.Name,
		Namespace:    target.pod.Namespace,
		Container:    target.container,
		Timestamp:    parsed.Timestamp.UnixNano(),
		Message:      parsed.Message,
		Level:        parsed.Level,
		Labels:       FilterPodLabels(target.pod.Labels),
		Raw:          parsed.Raw,
		Workload:     workload.Name,
		WorkloadKind: workload.Kind,
		NodeName:     target.pod.Spec.NodeName,
		Truncated:    parsed.Truncated,
	}
}

// maxK8sLogPrefixBytes bounds the "2006-01-02T15:04:05.999999999Z " prefix the
// pods/log API puts in front of every line. RFC3339Nano is at most 35 bytes
// (offset form, full nanoseconds) plus the separating space; 64 is comfortable
// headroom on a cap measured in hundreds of kilobytes.
const maxK8sLogPrefixBytes = 64

// readerCap converts the payload limit into the transport-line limit the
// reader enforces. A non-positive limit stays non-positive: zero means "no rule
// pushed" and must not become a cap of 64.
func readerCap(maxLineBytes int) int {
	if maxLineBytes <= 0 {
		return maxLineBytes
	}
	return maxLineBytes + maxK8sLogPrefixBytes
}

func (c *Collector) handleLogLine(ctx context.Context, target streamTarget, parsed ParsedLine) error {
	// The authoritative cut. The reader caps a single line, but the joiner
	// concatenates a stack trace from many of them (maxJoinedBytes is 64 KB),
	// so a joined record can exceed a max_line_bytes configured below that.
	// Only here are the bytes that will actually ship.
	rules := c.rules.Get()
	if rules.MaxLineBytes > 0 {
		if len(parsed.Raw) > rules.MaxLineBytes {
			parsed.Raw = trimToRune(parsed.Raw[:rules.MaxLineBytes], true)
			parsed.Truncated = true
		}
		// Message is cut separately, not derived from the Raw cut. For a plain
		// line the two hold the same bytes, but for a JSON line Message is the
		// extracted `message` field while Raw is the whole document, so one can
		// be over the limit while the other is not.
		//
		// Cutting only Raw is what shipped until 2026-08-19, and the consumer
		// indexes Message: the gateway logged message_bytes 262177 against a
		// negotiated max_line_bytes of 262144. That is the reader's cap
		// (max_line_bytes + maxK8sLogPrefixBytes) less the 31-byte RFC3339Nano
		// prefix -- i.e. every over-long line shipped at the reader's cap, and
		// the limit bound nothing on the field that is actually queried.
		if len(parsed.Message) > rules.MaxLineBytes {
			parsed.Message = string(trimToRune([]byte(parsed.Message[:rules.MaxLineBytes]), true))
			parsed.Truncated = true
		}
	}
	if parsed.Truncated {
		c.metrics.incTruncated(target.pod.Namespace, target.pod.Name)
		c.counters.IncTruncated()
	}

	// The per-stream limiter runs BEFORE the queue write: the whole point is
	// that bytes Loki would answer 429 to never reach the wire or the disk
	// buffer. It is correct here in a way a tenant-wide limiter could not be --
	// one Loki stream lives entirely on this one agent.
	workload := c.workloads.Resolve(ctx, target.pod)
	key := ingestrules.StreamKey(target.pod.Namespace, target.pod.Name, target.container,
		LevelLabel(parsed.Level), workload.Name, workload.Kind)
	if !c.limiter.Allow(key, len(parsed.Raw), rules, time.Now()) {
		c.metrics.incDropped(target.pod.Namespace, target.pod.Name, "rate_limited")
		c.counters.IncRateLimited()
		return nil
	}

	entry := buildLogEntry(target, parsed, workload)

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
