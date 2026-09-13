package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// The node console's helper Pod. Kubernetes has no exec for a Node, so the
// agent creates a privileged Pod pinned to the node, waits for it to run,
// and execs `nsenter -t 1 ...` into it through the same pods/exec session
// Phase B built. Everything here is about creating that Pod and, above
// all, removing it: a helper left behind is a privileged Pod nobody is
// watching. Three layers remove it -- activeDeadlineSeconds (the API
// server, no agent involved), the ownerReference to the agent Pod (garbage
// collection when the agent is replaced) and the agent's own delete on
// session end plus SweepHelpers at boot.

const (
	// HelperLabelName is the app.kubernetes.io/name every helper carries;
	// SweepHelpers selects on it.
	HelperLabelName    = "kubexa-node-shell"
	helperSessionLabel = "kubexa.dev/session-id"
	helperNodeLabel    = "kubexa.dev/node"
	helperContainer    = "shell"
	helperNamePrefix   = "kubexa-node-shell-"

	// helperDeadlineSlack is added to max_session_sec for the Pod's
	// activeDeadlineSeconds, so the API server's kill is the backstop and
	// the agent's own delete the normal ending.
	helperDeadlineSlack = time.Minute
	// helperDeleteTimeout bounds the delete that runs after a session
	// ends, on a context detached from the session's.
	helperDeleteTimeout = 10 * time.Second
	// helperPollInterval is how often awaitHelperRunning re-reads the Pod.
	helperPollInterval = 500 * time.Millisecond
)

// OwnPod identifies the agent's own Pod, the ownerReference target.
type OwnPod struct {
	Name      string
	Namespace string
	UID       types.UID
}

// ResolveOwnPod reads the agent's own Pod once. name and namespace come
// from the downward API (POD_NAME, POD_NAMESPACE); an empty name means the
// chart did not render them, which is an error here so the caller can
// disable the node console with a logged reason rather than create
// ownerless helpers by accident.
func ResolveOwnPod(ctx context.Context, cs kubernetes.Interface, name, namespace string) (OwnPod, error) {
	if name == "" || namespace == "" {
		return OwnPod{}, errors.New("POD_NAME and POD_NAMESPACE are not set; the chart renders them from the downward API")
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return OwnPod{}, fmt.Errorf("get own pod %s/%s: %w", namespace, name, err)
	}
	return OwnPod{Name: p.Name, Namespace: p.Namespace, UID: p.UID}, nil
}

// helperError is a helper-Pod failure the session reports as an ExecExit.
type helperError struct {
	Reason agentv1.ExecExitReason
	Msg    string
}

func (e *helperError) Error() string { return e.Msg }

func rejected(format string, args ...any) *helperError {
	return &helperError{Reason: agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED, Msg: fmt.Sprintf(format, args...)}
}

type helperSpec struct {
	sessionID  string
	node       string
	namespace  string
	image      string
	owner      *OwnPod // nil when the namespace is not the agent's own
	maxSession time.Duration
}

// helperName derives the Pod name from the session id so a retried create
// is idempotent and the sweep log can name the session it removes.
func helperName(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return helperNamePrefix + hex.EncodeToString(sum[:])[:8]
}

func helperPod(s helperSpec) *corev1.Pod {
	deadline := int64((s.maxSession + helperDeadlineSlack) / time.Second)
	privileged := true
	noToken := false
	noLinks := false
	grace := int64(1)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperName(s.sessionID),
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       HelperLabelName,
				"app.kubernetes.io/managed-by": "kubexa-agent",
				helperSessionLabel:             s.sessionID,
				helperNodeLabel:                s.node,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                      s.node,
			HostPID:                       true,
			HostNetwork:                   true,
			HostIPC:                       true,
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &noToken,
			EnableServiceLinks:            &noLinks,
			Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			ActiveDeadlineSeconds:         &deadline,
			TerminationGracePeriodSeconds: &grace,
			Containers: []corev1.Container{{
				Name:            helperContainer,
				Image:           s.image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceMemory: resourceMemory256Mi(),
				}},
			}},
		},
	}
	if s.owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: s.owner.Name, UID: s.owner.UID,
		}}
	}
	return p
}

// createHelper creates the Pod. AlreadyExists is success (a retried open
// under the same session id). Every refusal -- RBAC on pods create, an
// admission policy, a quota -- is HELPER_REJECTED with the API server's
// own message, which is the text that names what refused it.
func createHelper(ctx context.Context, cs kubernetes.Interface, s helperSpec) (*corev1.Pod, error) {
	p, err := cs.CoreV1().Pods(s.namespace).Create(ctx, helperPod(s), metav1.CreateOptions{})
	if err == nil {
		return p, nil
	}
	if apierrors.IsAlreadyExists(err) {
		return cs.CoreV1().Pods(s.namespace).Get(ctx, helperName(s.sessionID), metav1.GetOptions{})
	}
	return nil, rejected("helper pod create refused: %v", err)
}

// awaitHelperRunning polls the Pod until Running, or ends early on a
// container waiting reason that will not resolve on its own (an image
// that cannot be pulled, a container that cannot be created) or a Pod
// that already failed, or on timeout. Polling rather than a watch: the
// wait is bounded and short, and a poll is what the fake clientset in the
// tests answers without reactor plumbing.
func awaitHelperRunning(ctx context.Context, cs kubernetes.Interface, namespace, name string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return rejected("helper pod lookup failed: %v", err)
		}
		switch p.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return rejected("helper pod ended before the shell opened (phase %s): %s", p.Status.Phase, p.Status.Message)
		}
		for _, cst := range p.Status.ContainerStatuses {
			if w := cst.State.Waiting; w != nil {
				switch w.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerError",
					"CreateContainerConfigError", "RunContainerError":
					return rejected("helper pod container cannot start: %s: %s", w.Reason, w.Message)
				}
			}
		}
		if time.Now().After(deadline) {
			return rejected("helper pod not running after %s (phase %s)", timeout, p.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return rejected("helper pod wait canceled: %v", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// deleteHelper removes the Pod; NotFound is success (the API server's
// activeDeadlineSeconds or the garbage collector got there first).
func deleteHelper(ctx context.Context, cs kubernetes.Interface, namespace, name string) error {
	policy := metav1.DeletePropagationBackground
	err := cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// SweepHelpers deletes every helper Pod in the namespace. It runs at boot,
// when every helper is an orphan by definition: no session survives an
// agent restart. Best-effort -- a list error is logged and boot goes on.
// Returns how many it deleted.
func SweepHelpers(ctx context.Context, cs kubernetes.Interface, namespace string, log *logger.Logger) int {
	if log == nil {
		log = logger.New("exec")
	}
	list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + HelperLabelName,
	})
	if err != nil {
		log.Err(err).Warn("node shell sweep: list helpers", logger.F("namespace", namespace))
		return 0
	}
	n := 0
	for _, p := range list.Items {
		if err := deleteHelper(ctx, cs, namespace, p.Name); err != nil {
			log.Err(err).Warn("node shell sweep: delete", logger.F("pod", p.Name))
			continue
		}
		n++
		log.Info("node shell sweep: removed orphan helper",
			logger.F("pod", p.Name), logger.F("session_id", p.Labels[helperSessionLabel]))
	}
	return n
}

func resourceMemory256Mi() resource.Quantity { return resource.MustParse("256Mi") }
