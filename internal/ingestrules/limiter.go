package ingestrules

import (
	"strings"
	"sync"
	"time"
)

// MaxLimiterEntries bounds the bucket map. One entry is one Loki stream, and
// the stream identity includes the pod name -- so every rollout, every crash
// loop and every minutely Job mints new entries.
const MaxLimiterEntries = 20_000

// BucketTTL is how long an idle bucket is kept. A pod that stopped logging
// keeps no budget.
const BucketTTL = 10 * time.Minute

type bucket struct {
	tokens float64
	last   time.Time
	used   time.Time
}

// Limiter is a per-stream token bucket.
//
// It is correct here in a way a tenant-wide limiter could never be: one Loki
// stream lives entirely on one agent, because the chart pins replicas to 1 and
// there is no leader election. The tenant-wide ingestion rate is the sum
// across every cluster the tenant runs and stays with the consumer.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewLimiter builds a Limiter.
func NewLimiter() *Limiter {
	return &Limiter{buckets: make(map[string]*bucket)}
}

// Allow reports whether n bytes may be sent on the stream named by key, at the
// instant `at`.
//
// A zero rate or a zero burst means no rule was pushed, and no rule means no
// limit -- never a limit of zero.
func (l *Limiter) Allow(key string, n int, r Rules, at time.Time) bool {
	if l == nil || r.PerStreamRate <= 0 || r.PerStreamBurst <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		// Past the cap the limiter stops tracking and admits. It must never
		// become the reason for the data loss it exists to prevent, and a map
		// this large already means something upstream is wrong.
		if len(l.buckets) >= MaxLimiterEntries {
			return true
		}
		b = &bucket{tokens: float64(r.PerStreamBurst), last: at}
		l.buckets[key] = b
	}

	if elapsed := at.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * float64(r.PerStreamRate)
		if b.tokens > float64(r.PerStreamBurst) {
			b.tokens = float64(r.PerStreamBurst)
		}
		b.last = at
	}
	b.used = at

	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// Evict drops buckets idle since before the given instant.
func (l *Limiter) Evict(before time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.used.Before(before) {
			delete(l.buckets, k)
		}
	}
}

// Tracked is how many buckets are live. Exported for the eviction and cap
// tests; nothing in production reads it.
func (l *Limiter) Tracked() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// StreamKey renders the stream identity kubexa-consumer builds, minus the two
// fields that are constant for one agent (tenant_id and cluster_id).
//
// THIS MUST MATCH kubexa-consumer/internal/writer/loki/writer.go's rawLabels
// map. If that map gains or loses a field, these buckets stop corresponding to
// Loki's streams and nothing fails loudly -- the limiter simply meters the
// wrong thing. The separator is NUL because no label value can contain one, so
// two different identities cannot collide into one key.
func StreamKey(namespace, pod, container, level, workload, workloadKind string) string {
	return strings.Join([]string{namespace, pod, container, level, workload, workloadKind}, "\x00")
}
