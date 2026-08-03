package logs

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// workloadRef is the owning workload of a pod: what stays the same across the
// pod names a rollout burns through.
type workloadRef struct {
	Name string
	Kind string
}

// workloadResolver maps a pod to its workload, walking one level up where the
// direct owner is an implementation detail (ReplicaSet, and the Job a CronJob
// created). Results are cached: the answer changes only when a new controller
// appears, and the alternative is an API call per log line.
type workloadResolver struct {
	client kubernetes.Interface
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]cachedRef
}

type cachedRef struct {
	ref workloadRef
	at  time.Time
}

func newWorkloadResolver(client kubernetes.Interface, ttl time.Duration) *workloadResolver {
	return &workloadResolver{client: client, ttl: ttl, cache: map[string]cachedRef{}}
}

func (r *workloadResolver) Resolve(ctx context.Context, pod *corev1.Pod) workloadRef {
	owner := controllerOf(pod.OwnerReferences)
	if owner == nil {
		return workloadRef{}
	}
	direct := workloadRef{Name: owner.Name, Kind: owner.Kind}
	if owner.Kind != "ReplicaSet" && owner.Kind != "Job" {
		return direct
	}

	key := pod.Namespace + "/" + owner.Kind + "/" + owner.Name
	if ref, ok := r.cached(key); ok {
		return ref
	}

	resolved := r.lookup(ctx, pod.Namespace, direct)
	r.store(key, resolved)
	return resolved
}

// lookup walks one level up. A failed lookup returns the direct owner rather
// than nothing: an unresolvable ReplicaSet name is still more useful than an
// empty label, and it is cached so a missing RBAC verb costs one call per
// controller, not one per line.
func (r *workloadResolver) lookup(ctx context.Context, namespace string, direct workloadRef) workloadRef {
	switch direct.Kind {
	case "ReplicaSet":
		rs, err := r.client.AppsV1().ReplicaSets(namespace).Get(ctx, direct.Name, metav1.GetOptions{})
		if err != nil {
			return direct
		}
		if owner := controllerOf(rs.OwnerReferences); owner != nil {
			return workloadRef{Name: owner.Name, Kind: owner.Kind}
		}
	case "Job":
		job, err := r.client.BatchV1().Jobs(namespace).Get(ctx, direct.Name, metav1.GetOptions{})
		if err != nil {
			return direct
		}
		if owner := controllerOf(job.OwnerReferences); owner != nil {
			return workloadRef{Name: owner.Name, Kind: owner.Kind}
		}
	}
	return direct
}

func (r *workloadResolver) cached(key string) (workloadRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok || time.Since(entry.at) > r.ttl {
		return workloadRef{}, false
	}
	return entry.ref, true
}

func (r *workloadResolver) store(key string, ref workloadRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = cachedRef{ref: ref, at: time.Now()}
}

func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}
