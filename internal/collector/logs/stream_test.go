package logs

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

func TestStreamCursor_markProcessed(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	ts1 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ts2 := time.Date(2024, 6, 1, 12, 0, 1, 0, time.UTC)
	tsOlder := time.Date(2024, 6, 1, 11, 59, 0, 0, time.UTC)

	cur.markProcessed(ts1)
	cur.markProcessed(tsOlder)
	cur.markProcessed(ts2)

	since, ok := cur.sinceForReconnect(reconnectSinceOverlap)
	if !ok {
		t.Fatal("sinceForReconnect() = false, want true")
	}
	want := ts2.Add(-reconnectSinceOverlap)
	if !since.Equal(want) {
		t.Fatalf("since = %v, want %v", since, want)
	}
}

func TestStreamCursor_sinceForReconnect_noProgress(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	if _, ok := cur.sinceForReconnect(reconnectSinceOverlap); ok {
		t.Fatal("sinceForReconnect() = true before any lines, want false")
	}
}

func TestStreamCursor_sinceForReconnect_zeroOverlap(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	cur.markProcessed(ts)

	since, ok := cur.sinceForReconnect(0)
	if !ok {
		t.Fatal("sinceForReconnect() = false, want true")
	}
	if !since.Equal(ts) {
		t.Fatalf("since = %v, want %v", since, ts)
	}
}

func TestStreamCursor_nilSafe(t *testing.T) {
	t.Parallel()

	var cur *streamCursor
	cur.markProcessed(time.Now())
	if _, ok := cur.sinceForReconnect(time.Second); ok {
		t.Fatal("nil cursor should not report resume point")
	}
}

func TestHandleLogLinePopulatesWorkload(t *testing.T) {
	ctrl := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "api-1", Namespace: "stage",
		OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &ctrl}},
	}, Spec: corev1.PodSpec{NodeName: "node-2"}}

	entry := buildLogEntry(streamTarget{pod: pod, container: "app"},
		ParsedLine{Message: "hi", Raw: []byte("hi"), Timestamp: time.Unix(0, 7)},
		workloadRef{Name: "db", Kind: "StatefulSet"})

	if entry.GetWorkload() != "db" || entry.GetWorkloadKind() != "StatefulSet" {
		t.Fatalf("workload = %q/%q", entry.GetWorkloadKind(), entry.GetWorkload())
	}
	if entry.GetNodeName() != "node-2" {
		t.Fatalf("node = %q", entry.GetNodeName())
	}
}

// recordingWriter is a test double for Writer that captures the entries it
// was handed and whether the context it received was already Done at call
// time — the exact thing a reused, canceled stream context would look like.
type recordingWriter struct {
	entries    []*agentv1.LogEntry
	sawDoneCtx bool
}

func (w *recordingWriter) Write(ctx context.Context, msg *agentv1.AgentMessage) error {
	if ctx.Err() != nil {
		w.sawDoneCtx = true
	}
	w.entries = append(w.entries, msg.GetLogs().GetEntries()...)
	return nil
}

// A stream tears down on more than reconnect: pod deletion, rule removal,
// and agent shutdown all cancel the stream's context with no further chance
// to resend. If the deferred flush reused that context, the record the
// joiner is still holding — often the tail of a multi-line stack trace
// inside the hold window — would be dropped instead of reaching the writer.
func TestFlushJoinerDeliversAPendingRecordAfterTheStreamContextIsCanceled(t *testing.T) {
	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	w := &recordingWriter{}
	c := &Collector{
		writer:       w,
		log:          logger.New("test"),
		metrics:      m,
		agentMeta:    &commonv1.AgentMetadata{},
		workloads:    newWorkloadResolver(fake.NewSimpleClientset(), time.Minute),
		writeTimeout: 50 * time.Millisecond,
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "stage"}}
	target := streamTarget{pod: pod, container: "app"}

	join := newJoiner(maxJoinedBytes, maxJoinHold)
	join.Add(ParsedLine{Message: "panic: boom", Raw: []byte("panic: boom"), Timestamp: time.Unix(0, 1)}, time.Now())

	streamCtx, cancel := context.WithCancel(context.Background())
	cancel() // teardown: the stream's own context is already Done.

	c.flushJoiner(streamCtx, c.log, target, join)

	if len(w.entries) != 1 {
		t.Fatalf("writes = %d, want 1 (pending record must still reach the writer)", len(w.entries))
	}
	if w.entries[0].GetMessage() != "panic: boom" {
		t.Fatalf("message = %q", w.entries[0].GetMessage())
	}
	if w.sawDoneCtx {
		t.Fatal("flushJoiner handed the writer a context that was already Done; it must detach from the stream context")
	}
}
