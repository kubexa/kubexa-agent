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

// Options configures a Manager.
type Options struct {
	Policy     *policy.Policy
	Clients    k8s.ExecClients
	Settings   config.PodExecSettings
	Logger     *logger.Logger
	Registerer prometheus.Registerer

	// newExecutor is the test seam; nil means remotecommand.NewSPDYExecutor.
	newExecutor func(*rest.Config, *url.URL) (remotecommand.Executor, error)
}

// Manager opens console sessions under the owner's policy and limits.
type Manager struct {
	opts Options
	log  *logger.Logger
	// execREST renders the pods/exec URL. It is built from Clients.REST --
	// the config the executor dials -- rather than taken from the clientset,
	// whose RESTClient() a fake clientset returns as nil.
	execREST rest.Interface

	mu       sync.Mutex
	sessions map[string]*Session
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
	pod := open.GetTarget().GetPod()
	if pod == nil {
		return nil, refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, "phase B answers pod targets only")
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
	if len(m.sessions) >= m.opts.Settings.MaxSessions {
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
	p, err := m.opts.Clients.Clientset.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		release()
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
		s.run(ctx, ex)
		m.log.Info("console session ended",
			logger.F("session_id", id), logger.F("namespace", target.Namespace),
			logger.F("pod", target.Name), logger.F("container", target.Container),
			logger.F("reason", s.Exit().GetReason().String()), logger.F("code", s.Exit().GetCode()))
	}()
	m.log.Info("console session opened",
		logger.F("session_id", id), logger.F("namespace", target.Namespace),
		logger.F("pod", target.Name), logger.F("container", target.Container),
		logger.F("rule", d.RuleID))
	return s, nil
}

// clampSeconds takes the gateway's request but never more than the agent's
// own limit; a non-positive request means "the agent's limit".
func clampSeconds(requested int32, own int) time.Duration {
	if requested <= 0 || int(requested) > own {
		return time.Duration(own) * time.Second
	}
	return time.Duration(requested) * time.Second
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
