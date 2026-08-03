package logs

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestHandleLogLinePopulatesStreamAndWorkload(t *testing.T) {
	ctrl := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "api-1", Namespace: "stage",
		OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &ctrl}},
	}, Spec: corev1.PodSpec{NodeName: "node-2"}}

	entry := buildLogEntry(streamTarget{pod: pod, container: "app"},
		ParsedLine{Message: "hi", Raw: []byte("hi"), Stream: "stderr", Timestamp: time.Unix(0, 7)},
		workloadRef{Name: "db", Kind: "StatefulSet"})

	if entry.GetStream() != "stderr" {
		t.Fatalf("stream = %q", entry.GetStream())
	}
	if entry.GetWorkload() != "db" || entry.GetWorkloadKind() != "StatefulSet" {
		t.Fatalf("workload = %q/%q", entry.GetWorkloadKind(), entry.GetWorkload())
	}
	if entry.GetNodeName() != "node-2" {
		t.Fatalf("node = %q", entry.GetNodeName())
	}
}
