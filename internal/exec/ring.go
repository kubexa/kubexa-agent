package exec

import (
	"sync"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// Frame is one output chunk with its per-session sequence number.
type Frame struct {
	Channel agentv1.ExecChannel
	Seq     uint64
	Chunk   []byte
}

// ring keeps the most recent output frames, bounded by bytes, so a
// re-attaching stream can be replayed from the last seq the other side saw.
type ring struct {
	mu     sync.Mutex
	frames []Frame
	bytes  int
	max    int
	next   uint64 // seq of the next frame pushed; starts at 1
}

func newRing(maxBytes int) *ring {
	return &ring{max: maxBytes, next: 1}
}

// push copies chunk, assigns the next seq and evicts from the front until
// the buffer fits. A single chunk larger than max is kept alone.
func (r *ring) push(ch agentv1.ExecChannel, chunk []byte) Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := Frame{Channel: ch, Seq: r.next, Chunk: append([]byte(nil), chunk...)}
	r.next++
	r.frames = append(r.frames, f)
	r.bytes += len(f.Chunk)
	for r.bytes > r.max && len(r.frames) > 1 {
		r.bytes -= len(r.frames[0].Chunk)
		r.frames[0].Chunk = nil
		r.frames = r.frames[1:]
	}
	return f
}

// since returns the frames with Seq > after, in order, and whether any frame
// between after and the oldest retained one was evicted (a gap the receiver
// should be told about).
func (r *ring) since(after uint64) ([]Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frames) == 0 {
		return nil, after+1 < r.next
	}
	oldest := r.frames[0].Seq
	gap := after+1 < oldest
	out := make([]Frame, 0, len(r.frames))
	for _, f := range r.frames {
		if f.Seq > after {
			out = append(out, f)
		}
	}
	return out, gap
}

// last returns the highest seq pushed so far (0 if none).
func (r *ring) last() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next - 1
}
