package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// RegisterRuntimeCollectors adds the Go runtime and process collectors to reg.
//
// The agent builds its own registry instead of using prometheus.DefaultRegisterer,
// and a fresh registry carries none of these. That is not a cosmetic gap: a
// v0.6.0 agent was OOMKilled eight times under a 512Mi limit with no
// go_memstats_* to look at, so the live heap had to be inferred from cadvisor
// and the answer -- a GC goal pinned at twice the live heap -- was only
// reachable by reading an eleven-hour RSS curve.
//
// What the pair buys, specifically:
//
//   - go_memstats_heap_alloc_bytes is the LIVE heap; go_memstats_next_gc_bytes
//     is the goal the collector is steering toward. Read together they separate
//     "the program is holding more" from "the collector is aiming high", which
//     are different bugs with different fixes.
//   - process_resident_memory_bytes is what the kernel charges against the
//     container limit. Heap alone cannot tell you how close to death you are.
//
// Block and mutex profiling stay off: they carry a standing runtime cost, and
// this agent's whole point is to be cheap in the customer's cluster. Heap,
// goroutine and CPU profiles need no such flag.
//
// Registration is deliberately not idempotent -- a duplicate registration is a
// wiring mistake and must surface as an error rather than be swallowed.
func RegisterRuntimeCollectors(reg prometheus.Registerer) error {
	if reg == nil {
		return errors.New("metrics: nil registerer")
	}
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return err
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return err
	}
	return nil
}
