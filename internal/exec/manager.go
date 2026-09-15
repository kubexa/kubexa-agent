package exec

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubexa/kubexa-agent/internal/exec/policy"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// podLookupTimeout bounds the pod Get in Open. Open has reserved a
// max_sessions slot by then, and the gateway gives up on the attach at 15 s
// anyway, so a hung API server round trip must not pin the slot for TCP's
// own timeout. Same bound as the mutation executor's default.
const podLookupTimeout = 10 * time.Second

// Options configures a Manager.
type Options struct {
	Policy     *policy.Policy
	Clients    k8s.ExecClients
	Settings   config.PodExecSettings
	Logger     *logger.Logger
	Registerer prometheus.Registerer

	// Node is the node console's wiring; nil means exec.node is off and a
	// node target is refused as policy-denied.
	Node *NodeOptions

	// newExecutor is the test seam; nil means remotecommand.NewSPDYExecutor.
	newExecutor func(*rest.Config, *url.URL) (remotecommand.Executor, error)
	// podLookup overrides podLookupTimeout; zero means the const. Tests only.
	podLookup time.Duration
	// helperPoll overrides helperPollInterval; zero means the const. Tests only.
	helperPoll time.Duration
}

// NodeOptions is what the node console needs beyond the pod console's
// clients: its own policy and limits, where helpers go and, when that is
// the agent's own namespace, the agent Pod to own them.
type NodeOptions struct {
	Policy   *policy.NodePolicy
	Settings config.NodeExecSettings
	// Owner is the agent's own Pod; nil when Settings.Namespace names a
	// namespace other than the agent's (an ownerReference cannot cross
	// namespaces), in which case only the deadline, the delete on end and
	// the boot sweep remove helpers.
	Owner *OwnPod
	// Namespace is where helpers are created: Settings.Namespace if set,
	// else Owner.Namespace.
	Namespace string
}

// Manager opens console sessions under the owner's policy and limits.
type Manager struct {
	opts Options
	log  *logger.Logger
	// execREST renders the pods/exec URL. It is built from Clients.REST --
	// the config the executor dials -- rather than taken from the clientset,
	// whose RESTClient() a fake clientset returns as nil.
	execREST rest.Interface

	mu           sync.Mutex
	sessions     map[string]*Session
	nodeSessions int
}

// New builds a Manager. A nil Policy or Clientset is a wiring error.
func New(opts Options) (*Manager, error) {
	if opts.Policy == nil {
		return nil, errors.New("exec: policy is required")
	}
	if opts.Clients.Clientset == nil || opts.Clients.REST == nil {
		return nil, errors.New("exec: clients are required")
	}
	if opts.Logger == nil {
		opts.Logger = logger.New("exec")
	}
	if opts.newExecutor == nil {
		opts.newExecutor = func(c *rest.Config, u *url.URL) (remotecommand.Executor, error) {
			return remotecommand.NewSPDYExecutor(c, "POST", u)
		}
	}
	core, err := corev1client.NewForConfig(opts.Clients.REST)
	if err != nil {
		return nil, fmt.Errorf("exec: core/v1 client: %w", err)
	}
	return &Manager{opts: opts, log: opts.Logger, execREST: core.RESTClient(), sessions: map[string]*Session{}}, nil
}

// Settings returns the limits this agent applies.
func (m *Manager) Settings() config.PodExecSettings { return m.opts.Settings }

type podTarget struct{ Namespace, Name, Container string }

// Open starts one session, or returns the ExecExit that refuses it. A
// refusal never starts a process; the transport still attaches and sends
// the exit so the gateway learns WHY (there is no reply channel on Connect).
//
// Order: target shape, session cap, pod lookup (which also resolves the
// default container), policy, then the process. The pod lookup precedes the
// policy because the policy decides on a CONTAINER name and an empty one
// would otherwise be refused by any containers: allowlist -- see
// policy.Decide.
func (m *Manager) Open(ctx context.Context, open *agentv1.ExecOpen) (*Session, *agentv1.ExecExit) {
	if n := open.GetTarget().GetNode(); n != nil {
		return m.openNode(ctx, open, n)
	}
	pod := open.GetTarget().GetPod()
	if pod == nil {
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "exec_open carries no target")
	}
	id := strings.TrimSpace(open.GetSessionId())
	if id == "" {
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "session_id is required")
	}

	m.mu.Lock()
	if _, dup := m.sessions[id]; dup {
		m.mu.Unlock()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "duplicate session id")
	}
	// Node sessions share m.sessions (resume needs them there) but have
	// their own cap (m.nodeSessions against Node.Settings.MaxSessions), so
	// they must not count against the pod cap.
	if len(m.sessions)-m.nodeSessions >= m.opts.Settings.MaxSessions {
		m.mu.Unlock()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_TOO_MANY_SESSIONS,
			fmt.Sprintf("exec.pod.max_sessions (%d) reached", m.opts.Settings.MaxSessions))
	}
	// Reserve the slot under the lock; released on Done below.
	placeholder := &Session{}
	m.sessions[id] = placeholder
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}

	target := podTarget{Namespace: pod.GetNamespace(), Name: pod.GetName(), Container: pod.GetContainer()}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, m.podLookupTimeout())
	p, err := m.opts.Clients.Clientset.CoreV1().Pods(target.Namespace).Get(lookupCtx, target.Name, metav1.GetOptions{})
	lookupCancel()
	if err != nil {
		release()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "pod lookup timed out")
		}
		return nil, refuseFromAPIError(err)
	}
	target.Container, err = resolveContainer(p, target.Container)
	if err != nil {
		release()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_NOT_FOUND, err.Error())
	}

	d := m.opts.Policy.Decide(target.Namespace, target.Name, target.Container)
	if !d.Allowed {
		release()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED, d.Reason)
	}

	command := open.GetCommand()
	if len(command) == 0 {
		command = m.opts.Settings.DefaultShell
	}
	req := m.execREST.Post().
		Resource("pods").Namespace(target.Namespace).Name(target.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: target.Container,
			Command:   command,
			Stdin:     open.GetStdin(),
			Stdout:    true,
			Stderr:    !open.GetTty(),
			TTY:       open.GetTty(),
		}, scheme.ParameterCodec)
	ex, err := m.opts.newExecutor(m.opts.Clients.REST, req.URL())
	if err != nil {
		release()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, err.Error())
	}

	spec := sessionSpec{
		id:           id,
		tty:          open.GetTty(),
		stdin:        open.GetStdin(),
		resumeWindow: clampSeconds(open.GetResumeWindowSec(), m.opts.Settings.ResumeWindowSec),
		maxSession:   clampSeconds(open.GetMaxSessionSec(), m.opts.Settings.MaxSessionSec),
		ring:         newRing(RingBytes),
	}
	s := newSession(spec)
	s.target = target
	s.ruleID = d.RuleID

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	go func() {
		defer release()
		s.run(ctx, readyExecutor(ex))
		m.log.Info("console session ended",
			logger.F("session_id", id), logger.F("target_kind", "pod"), logger.F("namespace", target.Namespace),
			logger.F("pod", target.Name), logger.F("container", target.Container),
			logger.F("reason", s.Exit().GetReason().String()), logger.F("code", s.Exit().GetCode()))
	}()
	m.log.Info("console session opened",
		logger.F("session_id", id), logger.F("target_kind", "pod"), logger.F("namespace", target.Namespace),
		logger.F("pod", target.Name), logger.F("container", target.Container),
		logger.F("rule", d.RuleID))
	return s, nil
}

func (m *Manager) podLookupTimeout() time.Duration {
	if m.opts.podLookup > 0 {
		return m.opts.podLookup
	}
	return podLookupTimeout
}

// NodeReady reports whether this agent answers node targets: the section
// is wired and at least one node pattern is configured. The handshake's
// exec_node capability reads it (through Transport.NodeConsoleReady).
func (m *Manager) NodeReady() bool {
	return m != nil && m.opts.Node != nil && m.opts.Node.Policy.AllowsAnyNode()
}

func (m *Manager) helperPollInterval() time.Duration {
	if m.opts.helperPoll > 0 {
		return m.opts.helperPoll
	}
	return helperPollInterval
}

// clampSeconds takes the gateway's request but never more than the agent's
// own limit; a non-positive request means "the agent's limit".
func clampSeconds(requested int32, own int) time.Duration {
	if requested <= 0 || int(requested) > own {
		return time.Duration(own) * time.Second
	}
	return time.Duration(requested) * time.Second
}

// nodeMaxSession is clampSeconds for a node open, floored at the helper
// wait: the session clock starts at open and the helper wait runs under
// it, so a requested max_session shorter than helper_ready_timeout_sec
// could only ever end as MAX_SESSION under a "starting helper" line.
// Config validation already holds the agent's own limit at or above the
// wait, so the floor never exceeds own.
func nodeMaxSession(requested int32, s config.NodeExecSettings) time.Duration {
	d := clampSeconds(requested, s.MaxSessionSec)
	if wait := time.Duration(s.HelperReadyTimeoutSec) * time.Second; d < wait && wait <= time.Duration(s.MaxSessionSec)*time.Second {
		return wait
	}
	return d
}

func resolveContainer(p *corev1.Pod, requested string) (string, error) {
	if requested != "" {
		for _, c := range p.Spec.Containers {
			if c.Name == requested {
				return requested, nil
			}
		}
		return "", fmt.Errorf("container %q not found in pod %s/%s", requested, p.Namespace, p.Name)
	}
	if def := p.Annotations[defaultContainerAnnotation]; def != "" {
		for _, c := range p.Spec.Containers {
			if c.Name == def {
				return def, nil
			}
		}
	}
	if len(p.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod %s/%s has no containers", p.Namespace, p.Name)
	}
	return p.Spec.Containers[0].Name, nil
}

func refuse(reason agentv1.ExecExitReason, msg string) *agentv1.ExecExit {
	return &agentv1.ExecExit{Code: -1, Reason: reason, Message: msg}
}

// refuseFromAPIError maps the pod lookup's failure the way the mutation
// executor maps its errors: a 403 is RBAC_DENIED (the ServiceAccount lacks
// pods get -- rbac.exec in the chart grants it), a 404 is NOT_FOUND.
func refuseFromAPIError(err error) *agentv1.ExecExit {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_RBAC_DENIED, err.Error())
	case apierrors.IsNotFound(err):
		return refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_NOT_FOUND, err.Error())
	default:
		return refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, err.Error())
	}
}

// nsenterArgs enters PID 1's mount, UTS, IPC, network and PID namespaces
// -- the host's -- and runs the configured shell there. "-t 1" is why the
// helper needs hostPID.
var nsenterArgs = []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--"}

// openNode is Open for a node target: node lookup and policy
// synchronously, then the session is returned and its prepare step creates
// the helper Pod, waits for it, and builds the same pods/exec executor a
// pod console uses -- against the helper. The gateway's command is
// ignored: the apiserver refuses a non-empty one before it gets here, and
// what runs on a node is the operator's exec.node.shell, never a client's
// argv.
//
// Helper lifecycle, in layers. In-process, every ending of the session --
// a refusal inside prepare, exit, close, deadline, resume window elapsed
// -- runs the one deferred delete on the session goroutine. What
// this function does NOT cover is the agent dying mid-session: there is no
// shutdown hook, and a SIGTERM ends the run goroutine without its defers.
// That helper is left to the cluster -- the ownerReference on the agent Pod
// (garbage-collected once the Pod is replaced), the next boot's SweepHelpers
// (same namespace, see cmd/agent), or the Pod's own activeDeadlineSeconds
// (max_session + 60 s) -- whichever fires first.
func (m *Manager) openNode(ctx context.Context, open *agentv1.ExecOpen, target *agentv1.NodeTarget) (*Session, *agentv1.ExecExit) {
	no := m.opts.Node
	if no == nil || !no.Policy.AllowsAnyNode() {
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED,
			"node console is disabled in this agent's configuration")
	}
	id := strings.TrimSpace(open.GetSessionId())
	if id == "" {
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "session_id is required")
	}
	nodeName := strings.TrimSpace(target.GetName())
	if nodeName == "" {
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "node name is required")
	}

	m.mu.Lock()
	if _, dup := m.sessions[id]; dup {
		m.mu.Unlock()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "duplicate session id")
	}
	if m.nodeSessions >= no.Settings.MaxSessions {
		m.mu.Unlock()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_TOO_MANY_SESSIONS,
			fmt.Sprintf("exec.node.max_sessions (%d) reached", no.Settings.MaxSessions))
	}
	m.nodeSessions++
	m.sessions[id] = &Session{}
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		delete(m.sessions, id)
		m.nodeSessions--
		m.mu.Unlock()
	}

	lookupCtx, lookupCancel := context.WithTimeout(ctx, m.podLookupTimeout())
	_, err := m.opts.Clients.Clientset.CoreV1().Nodes().Get(lookupCtx, nodeName, metav1.GetOptions{})
	lookupCancel()
	if err != nil {
		release()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "node lookup timed out")
		}
		return nil, refuseFromAPIError(err)
	}
	d := no.Policy.Decide(nodeName)
	if !d.Allowed {
		release()
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED, d.Reason)
	}

	maxSession := nodeMaxSession(open.GetMaxSessionSec(), no.Settings)
	spec := helperSpec{sessionID: id, node: nodeName, namespace: no.Namespace, image: no.Settings.Image,
		owner: no.Owner, maxSession: maxSession}
	cs := m.opts.Clients.Clientset

	s := newSession(sessionSpec{
		id:           id,
		tty:          open.GetTty(),
		stdin:        open.GetStdin(),
		resumeWindow: clampSeconds(open.GetResumeWindowSec(), m.opts.Settings.ResumeWindowSec),
		maxSession:   maxSession,
		ring:         newRing(RingBytes),
	})
	s.target = podTarget{Namespace: no.Namespace, Name: helperName(id), Container: helperContainer}
	s.node = nodeName
	s.ruleID = d.RuleID

	// helperCleanup deletes the helper on a context of its own: the
	// session's may already be cancelled, and a cancelled delete is a
	// leaked privileged Pod until activeDeadlineSeconds.
	helperCleanup := func(name string) {
		dctx, cancel := context.WithTimeout(context.Background(), helperDeleteTimeout)
		defer cancel()
		if err := deleteHelper(dctx, cs, no.Namespace, name); err != nil {
			m.log.Err(err).Warn("node shell helper delete failed; activeDeadlineSeconds or the sweep will remove it",
				logger.F("session_id", id), logger.F("pod", name))
		}
	}

	// prepare runs on the session goroutine under run's max-session
	// context and resume watchdog: the transport has attached (or is
	// dialling) by the time the image pulls, so the gateway's attach
	// clock never sees this wait, and a browser that leaves mid-pull ends
	// it through Close or the resume window rather than at
	// helper_ready_timeout_sec. Nothing here deletes the helper: every
	// ending, refusal included, goes through the one deferred delete on
	// the session goroutine below.
	prepare := func(ctx context.Context) (remotecommand.Executor, *agentv1.ExecExit) {
		s.note("kubexa: starting node shell helper on " + nodeName + " (" + no.Settings.Image + ")...\r\n")
		helper, err := createHelper(ctx, cs, spec)
		if err != nil {
			return nil, refuseFromHelperError(err)
		}
		readyTimeout := time.Duration(no.Settings.HelperReadyTimeoutSec) * time.Second
		if err := awaitHelperRunning(ctx, cs, helper.Namespace, helper.Name, readyTimeout, m.helperPollInterval()); err != nil {
			return nil, refuseFromHelperError(err)
		}
		s.note("kubexa: helper ready, entering host namespaces\r\n")
		command := append(append([]string(nil), nsenterArgs...), no.Settings.Shell...)
		req := m.execREST.Post().
			Resource("pods").Namespace(helper.Namespace).Name(helper.Name).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: helperContainer,
				Command:   command,
				Stdin:     open.GetStdin(),
				Stdout:    true,
				Stderr:    !open.GetTty(),
				TTY:       open.GetTty(),
			}, scheme.ParameterCodec)
		ex, err := m.opts.newExecutor(m.opts.Clients.REST, req.URL())
		if err != nil {
			return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, err.Error())
		}
		return ex, nil
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	go func() {
		defer release()
		// The one delete for every ending. The helper may or may not exist
		// when run returns (a create refusal never made one; Close before
		// the goroutine was scheduled never ran prepare); deleteHelper
		// treats NotFound as success, so the unconditional delete costs
		// one harmless call in those cases and a real delete in the rest.
		defer helperCleanup(helperName(id))
		s.run(ctx, prepare)
		m.log.Info("console session ended",
			logger.F("session_id", id), logger.F("target_kind", "node"), logger.F("node", s.node),
			logger.F("helper_pod", helperName(id)),
			logger.F("reason", s.Exit().GetReason().String()), logger.F("code", s.Exit().GetCode()))
	}()
	m.log.Info("console session opened",
		logger.F("session_id", id), logger.F("target_kind", "node"), logger.F("node", s.node),
		logger.F("helper_pod", helperName(id)), logger.F("rule", d.RuleID))
	return s, nil
}

func refuseFromHelperError(err error) *agentv1.ExecExit {
	var he *helperError
	if errors.As(err, &he) {
		return refuse(he.Reason, he.Msg)
	}
	return refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, err.Error())
}
