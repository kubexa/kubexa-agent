package k8sresource

import (
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func entry(gvr schema.GroupVersionResource, kind agentv1.ResourceKind, aliases ...string) Descriptor {
	d := Descriptor{
		GVR:         gvr,
		ProtoKind:   kind,
		MetricLabel: gvr.Resource,
		SkipAdded:   gvr.Resource == "secrets",
	}
	for _, alias := range aliases {
		registry[alias] = d
	}
	return d
}

// registry maps lowercase aliases (plural/singular) to descriptors.
var registry = map[string]Descriptor{}

func init() {
	// core/v1
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, agentv1.ResourceKind_RESOURCE_KIND_POD, "pods", "pod")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}, agentv1.ResourceKind_RESOURCE_KIND_SERVICE, "services", "service")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}, agentv1.ResourceKind_RESOURCE_KIND_SECRET, "secrets", "secret")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, agentv1.ResourceKind_RESOURCE_KIND_CONFIGMAP, "configmaps", "configmap")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}, agentv1.ResourceKind_RESOURCE_KIND_NAMESPACE, "namespaces", "namespace")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}, agentv1.ResourceKind_RESOURCE_KIND_NODE, "nodes", "node")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}, agentv1.ResourceKind_RESOURCE_KIND_PERSISTENT_VOLUME_CLAIM, "persistentvolumeclaims", "persistentvolumeclaim", "pvc", "pvcs")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"}, agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, "persistentvolumes", "persistentvolume", "pv")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}, agentv1.ResourceKind_RESOURCE_KIND_SERVICE_ACCOUNT, "serviceaccounts", "serviceaccount")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "resourcequotas"}, agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, "resourcequotas", "resourcequota")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "limitranges"}, agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, "limitranges", "limitrange")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "replicationcontrollers"}, agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, "replicationcontrollers", "replicationcontroller")
	entry(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "endpoints"}, agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED, "endpoints", "endpoint")

	// apps/v1
	entry(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, agentv1.ResourceKind_RESOURCE_KIND_DEPLOYMENT, "deployments", "deployment")
	entry(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, agentv1.ResourceKind_RESOURCE_KIND_DAEMONSET, "daemonsets", "daemonset")
	entry(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, agentv1.ResourceKind_RESOURCE_KIND_REPLICASET, "replicasets", "replicaset")
	entry(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, agentv1.ResourceKind_RESOURCE_KIND_STATEFULSET, "statefulsets", "statefulset")

	// batch/v1
	entry(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, agentv1.ResourceKind_RESOURCE_KIND_JOB, "jobs", "job")
	entry(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, agentv1.ResourceKind_RESOURCE_KIND_CRONJOB, "cronjobs", "cronjob")

	// networking.k8s.io/v1
	entry(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, agentv1.ResourceKind_RESOURCE_KIND_INGRESS, "ingresses", "ingress")
	entry(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, agentv1.ResourceKind_RESOURCE_KIND_NETWORK_POLICY, "networkpolicies", "networkpolicy")

	// rbac.authorization.k8s.io/v1
	entry(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, agentv1.ResourceKind_RESOURCE_KIND_ROLE, "roles", "role")
	entry(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, agentv1.ResourceKind_RESOURCE_KIND_ROLE_BINDING, "rolebindings", "rolebinding")
	entry(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, agentv1.ResourceKind_RESOURCE_KIND_CLUSTER_ROLE, "clusterroles", "clusterrole")
	entry(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, agentv1.ResourceKind_RESOURCE_KIND_CLUSTER_ROLE_BINDING, "clusterrolebindings", "clusterrolebinding")

	// autoscaling/v2
	entry(schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}, agentv1.ResourceKind_RESOURCE_KIND_HORIZONTAL_POD_AUTOSCALER, "horizontalpodautoscalers", "horizontalpodautoscaler", "hpa", "hpas")

	// policy/v1
	entry(schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}, agentv1.ResourceKind_RESOURCE_KIND_POD_DISRUPTION_BUDGET, "poddisruptionbudgets", "poddisruptionbudget", "pdb", "pdbs")

	// storage.k8s.io/v1
	entry(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}, agentv1.ResourceKind_RESOURCE_KIND_STORAGE_CLASS, "storageclasses", "storageclass")

	// discovery.k8s.io/v1
	entry(schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}, agentv1.ResourceKind_RESOURCE_KIND_ENDPOINT_SLICE, "endpointslices", "endpointslice")
}

// KnownAliases returns all built-in lowercase aliases (for documentation/tests).
func KnownAliases() []string {
	out := make([]string, 0, len(registry))
	for alias := range registry {
		out = append(out, alias)
	}
	return out
}
