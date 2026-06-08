package k8sresource

import (
	"fmt"
	"strings"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Parse resolves a configured resource name to a Descriptor.
//
// Supported forms:
//   - Built-in alias: "jobs", "pod", "hpa"
//   - Core shorthand: "v1/pods"
//   - Full GVR: "batch/v1/jobs", "monitoring.coreos.com/v1/prometheusrules"
func Parse(name string) (Descriptor, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return Descriptor{}, fmt.Errorf("resource name must not be empty")
	}

	if d, ok := registry[key]; ok {
		return d, nil
	}

	if strings.Count(key, "/") >= 1 {
		return parseGVR(key)
	}

	return Descriptor{}, fmt.Errorf("unsupported resource %q (use a built-in alias or group/version/resource for CRDs)", name)
}

func parseGVR(spec string) (Descriptor, error) {
	parts := strings.Split(spec, "/")
	switch len(parts) {
	case 2:
		return descriptorForGVR(schema.GroupVersionResource{
			Group:    "",
			Version:  parts[0],
			Resource: parts[1],
		}), nil
	case 3:
		return descriptorForGVR(schema.GroupVersionResource{
			Group:    parts[0],
			Version:  parts[1],
			Resource: parts[2],
		}), nil
	default:
		return Descriptor{}, fmt.Errorf("invalid resource GVR %q: want group/version/resource or version/resource", spec)
	}
}

func descriptorForGVR(gvr schema.GroupVersionResource) Descriptor {
	if gvr.Resource == "" || gvr.Version == "" {
		return Descriptor{}
	}
	for _, d := range registry {
		if d.GVR.Group == gvr.Group && d.GVR.Version == gvr.Version && d.GVR.Resource == gvr.Resource {
			return d
		}
	}
	return Descriptor{
		GVR:         gvr,
		ProtoKind:   agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED,
		MetricLabel: gvr.Resource,
		SkipAdded:   gvr.Resource == "secrets",
	}
}

// ParseResourceKind maps a configured resource name to its proto ResourceKind.
func ParseResourceKind(name string) (agentv1.ResourceKind, error) {
	d, err := Parse(name)
	if err != nil {
		return agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, err
	}
	return d.ProtoKind, nil
}
