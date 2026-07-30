package state

import (
	"encoding/json"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const lastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

var jsonBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// SanitizeObjectMeta strips verbose or sensitive metadata fields.
//
// This scrubbing is unconditional -- it does NOT depend on the RedactSecrets setting, and
// it must stay that way. managedFields is pure noise on every object. The
// kubectl.kubernetes.io/last-applied-configuration annotation is more than noise: for a
// Secret created or updated with `kubectl apply`, that annotation holds a JSON copy of the
// full original manifest, including every base64-encoded value under .data/.stringData. So
// it is a second, independent copy of the secret payload that lives in metadata rather than
// in the Secret's data fields. If this were ever made conditional on RedactSecrets, an
// operator who turned stripping back on (RedactSecrets=true) would still leak every secret
// value through this annotation, defeating the setting entirely. Do not "simplify" this away.
func SanitizeObjectMeta(meta *metav1.ObjectMeta) {
	if meta == nil {
		return
	}
	meta.ManagedFields = nil
	if len(meta.Annotations) > 0 {
		delete(meta.Annotations, lastAppliedConfigAnnotation)
		if len(meta.Annotations) == 0 {
			meta.Annotations = nil
		}
	}
}

// SanitizeSecret sanitizes a typed Secret object. Metadata scrubbing (managedFields, the
// last-applied-configuration annotation) always happens via SanitizeObjectMeta, regardless
// of redactSecrets -- see the comment on SanitizeObjectMeta for why that must never become
// conditional. The Secret payload (Data/StringData) is only cleared when redactSecrets is
// true; by default (redactSecrets=false) values are left intact so the platform can serve
// them for cluster management purposes.
func SanitizeSecret(secret *corev1.Secret, redactSecrets bool) {
	if secret == nil {
		return
	}
	SanitizeObjectMeta(&secret.ObjectMeta)
	if !redactSecrets {
		return
	}
	secret.Data = nil
	secret.StringData = nil
}

// SanitizeUnstructured applies sanitization to a generic API object. Metadata scrubbing via
// sanitizeUnstructuredMetadata always happens, regardless of redactSecrets -- see the
// comment on SanitizeObjectMeta for why. The secrets-specific data/stringData removal below
// is only applied when redactSecrets is true.
func SanitizeUnstructured(obj *unstructured.Unstructured, pluralResource string, redactSecrets bool) {
	if obj == nil {
		return
	}
	sanitizeUnstructuredMetadata(obj)
	if redactSecrets && pluralResource == "secrets" {
		unstructured.RemoveNestedField(obj.Object, "data")
		unstructured.RemoveNestedField(obj.Object, "stringData")
	}
}

func sanitizeUnstructuredMetadata(obj *unstructured.Unstructured) {
	metadata, found, err := unstructured.NestedMap(obj.Object, "metadata")
	if !found || err != nil {
		return
	}
	delete(metadata, "managedFields")
	if ann, ok := metadata["annotations"].(map[string]any); ok {
		delete(ann, lastAppliedConfigAnnotation)
		if len(ann) == 0 {
			delete(metadata, "annotations")
		} else {
			metadata["annotations"] = ann
		}
	}
	_ = unstructured.SetNestedMap(obj.Object, metadata, "metadata")
}

// SanitizeRuntimeObject applies resource-specific sanitization to a deep-copied object.
// redactSecrets controls only the Secret payload (data/stringData); metadata scrubbing
// (managedFields, last-applied-configuration) always happens in both branches below,
// regardless of redactSecrets -- see the comment on SanitizeObjectMeta for why.
func SanitizeRuntimeObject(obj runtime.Object, pluralResource string, redactSecrets bool) {
	if obj == nil {
		return
	}
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		SanitizeUnstructured(o, pluralResource, redactSecrets)
	case *corev1.Secret:
		SanitizeSecret(o, redactSecrets)
	default:
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return
		}
		uns := &unstructured.Unstructured{Object: u}
		SanitizeUnstructured(uns, pluralResource, redactSecrets)
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u, obj)
	}
}

// MarshalObjectJSON sanitizes obj and encodes it as JSON. redactSecrets controls whether a
// Secret's data/stringData payload survives into the JSON; when false (the default), Secret
// values are preserved end to end so the platform can serve them for cluster management.
func MarshalObjectJSON(obj runtime.Object, pluralResource string, redactSecrets bool) ([]byte, error) {
	if obj == nil {
		return nil, nil
	}
	SanitizeRuntimeObject(obj, pluralResource, redactSecrets)
	enc, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	bufPtr := jsonBufPool.Get().(*[]byte)
	*bufPtr = append((*bufPtr)[:0], enc...)
	result := make([]byte, len(*bufPtr))
	copy(result, *bufPtr)
	jsonBufPool.Put(bufPtr)
	return result, nil
}

// ObjectLabels returns a copy of object labels.
func ObjectLabels(obj runtime.Object) map[string]string {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return nil
	}
	labels := acc.GetLabels()
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// ResourceMeta extracts namespace, name, UID, and resource version from a runtime object.
func ResourceMeta(obj runtime.Object) (namespace, name, uid, resourceVersion string) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return "", "", "", ""
	}
	return acc.GetNamespace(), acc.GetName(), string(acc.GetUID()), acc.GetResourceVersion()
}
