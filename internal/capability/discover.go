// Package capability discovers every API resource in the cluster and the
// agent's own permission to read each one, so the platform can present an
// honest resource type list instead of a hardcoded subset.
package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// GVR is one listable API resource, identified the way the dynamic client
// addresses it. Kind and Namespaced come along because the UI needs them and
// discovery already paid for them.
type GVR struct {
	Group      string
	Version    string
	Resource   string
	Kind       string
	Namespaced bool
}

// Discover returns every listable, non-subresource API resource in the
// cluster, plus the names of any API groups discovery could not reach.
//
// A partial result is success. When an aggregated API is unavailable — a down
// metrics-server, a broken APIService — ServerPreferredResources returns the
// resources it did reach alongside ErrGroupDiscoveryFailed. Treating that as
// fatal would let one unhealthy operator blank the entire catalog, so the
// unreachable groups are reported by name and the rest is kept.
func Discover(d discovery.DiscoveryInterface) ([]GVR, []string, error) {
	lists, err := d.ServerPreferredResources()

	var failedGroups []string
	if err != nil {
		var groupErr *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &groupErr) {
			return nil, nil, fmt.Errorf("discover server resources: %w", err)
		}
		for gv := range groupErr.Groups {
			failedGroups = append(failedGroups, gv.String())
		}
		sort.Strings(failedGroups)
	}

	out := make([]GVR, 0, 64)
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			continue
		}
		for _, r := range list.APIResources {
			if !isListableResource(r) {
				continue
			}
			out = append(out, GVR{
				Group:      gv.Group,
				Version:    gv.Version,
				Resource:   r.Name,
				Kind:       r.Kind,
				Namespaced: r.Namespaced,
			})
		}
	}
	return out, failedGroups, nil
}

// isListableResource drops what can never be probed usefully. Subresources
// ("pods/log") are not independently listable, and a resource whose own Verbs
// omit "list" cannot be listed by anyone regardless of RBAC — the API server
// has already answered, so spending an SSAR on it is waste.
func isListableResource(r metav1.APIResource) bool {
	if strings.Contains(r.Name, "/") {
		return false
	}
	for _, v := range r.Verbs {
		if v == "list" {
			return true
		}
	}
	return false
}

// Fingerprint hashes the GVR set so a refresh can tell "the cluster gained a
// CRD" from "the map iterated in a different order". Sorting first is what
// makes it order-independent.
func Fingerprint(gvrs []GVR) string {
	keys := make([]string, 0, len(gvrs))
	for _, g := range gvrs {
		keys = append(keys, g.Group+"/"+g.Version+"/"+g.Resource)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
