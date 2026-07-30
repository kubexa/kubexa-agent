package capability

import (
	"context"
	"sync"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authzv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

// Capability is one GVR plus what the agent may actually do with it.
type Capability struct {
	GVR
	CanList     bool
	CanWatch    bool
	ProbeFailed bool
}

const defaultProbeWorkers = 8

// Probe asks the API server, for each GVR, whether this agent may list and
// watch it across all namespaces.
//
// The answer comes from SelfSubjectAccessReview rather than from parsing our
// own RBAC: the API server is the authority, and a derived answer that is
// wrong produces exactly the silent UI lie this feature exists to prevent.
//
// A review that errors sets ProbeFailed instead of denying. The two mistakes
// are not symmetric — a wrong "allowed" surfaces as an error the user can
// report, while a wrong "denied" makes the resource vanish.
func Probe(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	gvrs []GVR,
	workers int,
) []Capability {
	if workers <= 0 {
		workers = defaultProbeWorkers
	}

	out := make([]Capability, len(gvrs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, g := range gvrs {
		wg.Add(1)
		go func(i int, g GVR) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			canList, listErr := allowed(ctx, authz, g, "list")
			canWatch, watchErr := allowed(ctx, authz, g, "watch")

			c := Capability{GVR: g}
			if listErr != nil || watchErr != nil {
				c.ProbeFailed = true
			} else {
				c.CanList = canList
				c.CanWatch = canWatch
			}
			out[i] = c
		}(i, g)
	}
	wg.Wait()
	return out
}

func allowed(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	g GVR,
	verb string,
) (bool, error) {
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				// Empty namespace means "in every namespace", which matches how
				// the agent reads: cluster-wide informers, not per-namespace.
				Namespace: metav1.NamespaceAll,
				Group:     g.Group,
				Version:   g.Version,
				Resource:  g.Resource,
				Verb:      verb,
			},
		},
	}
	res, err := authz.SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return res.Status.Allowed, nil
}
