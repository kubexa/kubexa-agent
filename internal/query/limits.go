package query

import "context"

const (
	defaultMaxInFlight = 4
	defaultMaxQueued   = 8
)

// gate bounds how many queries execute at once and how many may wait.
//
// The cap belongs here, in the agent, rather than upstream: it is the last
// line of defence for the customer's control plane, and anything on the
// Kubexa side is outside their trust boundary. A refresh-happy user should
// slow down their own queries, never the cluster.
type gate struct {
	slots chan struct{}
	queue chan struct{}
}

func newGate(inFlight, queued int) *gate {
	if inFlight <= 0 {
		inFlight = defaultMaxInFlight
	}
	if queued <= 0 {
		queued = defaultMaxQueued
	}
	return &gate{
		slots: make(chan struct{}, inFlight),
		queue: make(chan struct{}, inFlight+queued),
	}
}

// acquire takes an execution slot. It returns false immediately when the
// queue is already full, rather than blocking an unbounded number of
// goroutines on the stream's recv path.
func (g *gate) acquire(ctx context.Context) (release func(), ok bool) {
	select {
	case g.queue <- struct{}{}:
	default:
		return nil, false
	}
	select {
	case g.slots <- struct{}{}:
		return func() {
			<-g.slots
			<-g.queue
		}, true
	case <-ctx.Done():
		<-g.queue
		return nil, false
	}
}
