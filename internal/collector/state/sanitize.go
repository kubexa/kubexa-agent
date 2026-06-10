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

// SanitizeSecret clears secret payload fields; metadata and type are retained.
func SanitizeSecret(secret *corev1.Secret) {
	if secret == nil {
		return
	}
	SanitizeObjectMeta(&secret.ObjectMeta)
	secret.Data = nil
	secret.StringData = nil
}

// SanitizeUnstructured applies sanitization to a generic API object.
func SanitizeUnstructured(obj *unstructured.Unstructured, pluralResource string) {
	if obj == nil {
		return
	}
	sanitizeUnstructuredMetadata(obj)
	if pluralResource == "secrets" {
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
func SanitizeRuntimeObject(obj runtime.Object, pluralResource string) {
	if obj == nil {
		return
	}
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		SanitizeUnstructured(o, pluralResource)
	case *corev1.Secret:
		SanitizeSecret(o)
	default:
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return
		}
		uns := &unstructured.Unstructured{Object: u}
		SanitizeUnstructured(uns, pluralResource)
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u, obj)
	}
}

// MarshalObjectJSON sanitizes obj and encodes it as JSON.
func MarshalObjectJSON(obj runtime.Object, pluralResource string) ([]byte, error) {
	if obj == nil {
		return nil, nil
	}
	SanitizeRuntimeObject(obj, pluralResource)
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
