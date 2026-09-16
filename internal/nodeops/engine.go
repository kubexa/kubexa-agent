// internal/nodeops/engine.go
package nodeops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// Options builds an Engine.
type Options struct {
	Clientset kubernetes.Interface
	Policy    *Policy
	Own       OwnPod
	// MaxTimeout caps DrainOptions.timeout_sec (mutate.node.max_timeout_sec).
	MaxTimeout time.Duration
	// MinTimeout floors it. Default 30s; tests lower it.
	MinTimeout time.Duration
	// FixedTimeout bounds a cordon/uncordon. Default 30s.
	FixedTimeout time.Duration
	// PollInterval is the eviction/wait tick. Default 2s.
	PollInterval time.Duration
	// RetryInterval is how long a BLOCKED pod waits before the next eviction
	// attempt. Default 5s.
	RetryInterval time.Duration
	Logger        *logger.Logger
}

// Emitter receives every event a job produces, in order, on the job's own
// goroutine. It must not block for long: the loop that calls it is the one
// watching the deadline. An ALIAS, not a named type, so *Engine satisfies
// stream.NodeJobResponder (whose Start takes the bare func type).
type Emitter = func(*agentv1.NodeJobEvent)

// Engine runs at most ONE node job at a time (spec decision 8).
type Engine struct {
	opts    Options
	mu      sync.Mutex
	running *job
}

type job struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

// New builds an Engine.
func New(opts Options) (*Engine, error) {
	if opts.Clientset == nil {
		return nil, errors.New("nodeops: clientset is required")
	}
	if opts.Policy == nil {
		return nil, errors.New("nodeops: policy is required")
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 1800 * time.Second
	}
	if opts.MinTimeout <= 0 {
		opts.MinTimeout = 30 * time.Second
	}
	if opts.FixedTimeout <= 0 {
		opts.FixedTimeout = 30 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = logger.New("nodeops")
	}
	return &Engine{opts: opts}, nil
}

// Start decides the job (verb, policy, busy) and, when it may run, spawns
// it. It returns at once and ALWAYS emits at least one event: a refusal for
// anything it will not run, ACCEPTED (and then more) for anything it will.
//
// The job's context is NOT the gateway session's: a drain keeps evicting
// when the stream to the gateway drops, because a drain that stops because
// the control plane blinked is worse than one that finishes unseen (spec
// §2.3). Cancel and the deadline are the only two things that end it early.
//
// The slot is released BEFORE the terminal event is emitted, so a caller
// that starts the next job on seeing a terminal event is never told BUSY.
func (e *Engine) Start(req *agentv1.NodeJobRequest, emit Emitter) {
	if req == nil || emit == nil {
		return
	}
	id := req.GetJobId()
	verb, ok := VerbFromProto(req.GetVerb())
	if !ok {
		emit(refused(id, agentv1.NodeJobErrorCode_NODE_JOB_ERROR_INTERNAL, fmt.Sprintf("unknown node job verb %v", req.GetVerb())))
		return
	}
	if d := e.opts.Policy.Decide(req.GetNode(), verb); !d.Allowed {
		emit(refused(id, agentv1.NodeJobErrorCode_NODE_JOB_ERROR_POLICY_DENIED, d.Reason))
		return
	}

	e.mu.Lock()
	if e.running != nil {
		other := e.running.id
		e.mu.Unlock()
		emit(refused(id, agentv1.NodeJobErrorCode_NODE_JOB_ERROR_BUSY, "another node job ("+other+") is running on this agent"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.timeoutFor(verb, req.GetDrain()))
	j := &job{id: id, cancel: cancel, done: make(chan struct{})}
	e.running = j
	e.mu.Unlock()

	e.opts.Logger.Info("node job started",
		logger.F("job_id", id), logger.F("node", req.GetNode()), logger.F("verb", string(verb)),
		logger.F("dry_run", req.GetDrain().GetDryRun()))
	go func() {
		final := e.run(ctx, verb, req, emit)
		cancel()
		e.mu.Lock()
		if e.running == j {
			e.running = nil
		}
		e.mu.Unlock()
		close(j.done)
		e.opts.Logger.Info("node job finished",
			logger.F("job_id", id), logger.F("phase", final.GetPhase().String()),
			logger.F("error", final.GetError().GetCode().String()))
		emit(final)
	}()
}

// Cancel ends the running job if its id matches. It reports whether there
// was such a job; the CANCELLED event follows on the job's own goroutine.
func (e *Engine) Cancel(jobID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running == nil || e.running.id != jobID {
		return false
	}
	e.running.cancel()
	return true
}

// Running returns the running job's id, or "".
func (e *Engine) Running() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running == nil {
		return ""
	}
	return e.running.id
}

func (e *Engine) timeoutFor(verb Verb, d *agentv1.DrainOptions) time.Duration {
	if verb != VerbDrain {
		return e.opts.FixedTimeout
	}
	t := time.Duration(d.GetTimeoutSec()) * time.Second
	if t < e.opts.MinTimeout {
		t = e.opts.MinTimeout
	}
	if t > e.opts.MaxTimeout {
		t = e.opts.MaxTimeout
	}
	return t
}

// run executes the job and RETURNS its terminal event (the caller emits it
// after releasing the slot); intermediate events go through emit.
func (e *Engine) run(ctx context.Context, verb Verb, req *agentv1.NodeJobRequest, emit Emitter) *agentv1.NodeJobEvent {
	id, node := req.GetJobId(), req.GetNode()
	if _, err := e.opts.Clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); err != nil {
		// Cancel or the deadline firing during this call must end the job
		// CANCELLED/TIMEOUT, not REFUSED/INTERNAL -- no snapshot exists yet
		// this early, so build one just for endEarly.
		if ctx.Err() != nil {
			return endEarly(&snapshot{id: id}, ctx.Err())
		}
		code, msg := codeFor(err, "nodes get")
		return refused(id, code, msg)
	}
	switch verb {
	case VerbCordon, VerbUncordon:
		return e.runCordon(ctx, id, node, verb == VerbCordon, emit)
	case VerbDrain:
		return e.runDrain(ctx, id, node, req.GetDrain(), emit)
	default:
		return refused(id, agentv1.NodeJobErrorCode_NODE_JOB_ERROR_INTERNAL, "unreachable verb")
	}
}

func (e *Engine) runCordon(ctx context.Context, id, node string, cordon bool, emit Emitter) *agentv1.NodeJobEvent {
	s := &snapshot{id: id}
	emit(s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_ACCEPTED, ""))
	changed, err := SetUnschedulable(ctx, e.opts.Clientset, node, cordon, false)
	if err != nil {
		if ctx.Err() != nil {
			return endEarly(s, ctx.Err())
		}
		code, msg := codeFor(err, "nodes patch")
		return s.failed(code, msg)
	}
	s.nodeCordoned = cordon
	var msg string
	switch {
	case cordon && changed:
		msg = "node cordoned"
	case cordon:
		msg = "node already cordoned"
	case changed:
		msg = "node uncordoned"
	default:
		msg = "node already schedulable"
	}
	return s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED, msg)
}

// codeFor maps an API server error to a job error code and a message that
// names the verb the ClusterRole lacks (rbac.nodeOps in the chart).
func codeFor(err error, need string) (agentv1.NodeJobErrorCode, string) {
	switch {
	case apierrors.IsNotFound(err):
		return agentv1.NodeJobErrorCode_NODE_JOB_ERROR_NOT_FOUND, err.Error()
	case apierrors.IsForbidden(err):
		return agentv1.NodeJobErrorCode_NODE_JOB_ERROR_RBAC_DENIED,
			fmt.Sprintf("the agent's ClusterRole lacks %s (rbac.nodeOps): %v", need, err)
	default:
		return agentv1.NodeJobErrorCode_NODE_JOB_ERROR_INTERNAL, err.Error()
	}
}

func refused(id string, code agentv1.NodeJobErrorCode, msg string) *agentv1.NodeJobEvent {
	s := &snapshot{id: id}
	return s.refused(code, msg)
}

// snapshot is the job's whole state; every event is built from it.
type snapshot struct {
	id           string
	pods         []PodClass
	nodeCordoned bool
}

func (s *snapshot) event(phase agentv1.NodeJobPhase, msg string) *agentv1.NodeJobEvent {
	ev := &agentv1.NodeJobEvent{
		JobId:         s.id,
		Phase:         phase,
		Message:       msg,
		NodeCordoned:  s.nodeCordoned,
		EmittedUnixMs: time.Now().UnixMilli(),
		Pods:          make([]*agentv1.NodeJobPod, 0, len(s.pods)),
	}
	for _, p := range s.pods {
		ev.Pods = append(ev.Pods, &agentv1.NodeJobPod{Namespace: p.Namespace, Name: p.Name, State: p.State, Reason: p.Reason})
		switch p.State {
		case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_GONE:
			ev.PodsEvicted++
		case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED:
			ev.PodsSkipped++
		case agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING,
			agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING,
			agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED:
			ev.PodsPending++
		}
	}
	ev.PodsTotal = int32(len(s.pods))
	return ev
}

func (s *snapshot) failed(code agentv1.NodeJobErrorCode, msg string) *agentv1.NodeJobEvent {
	ev := s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED, msg)
	ev.Error = &agentv1.NodeJobError{Code: code, Message: msg}
	return ev
}

func (s *snapshot) refused(code agentv1.NodeJobErrorCode, msg string) *agentv1.NodeJobEvent {
	ev := s.event(agentv1.NodeJobPhase_NODE_JOB_PHASE_REFUSED, msg)
	ev.Error = &agentv1.NodeJobError{Code: code, Message: msg}
	return ev
}
