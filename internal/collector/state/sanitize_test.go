package state

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-creds",
			Namespace: "default",
			Annotations: map[string]string{
				lastAppliedConfigAnnotation: `{"apiVersion":"v1"}`,
				"other":                     "keep",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": []byte("secret")},
		StringData: map[string]string{
			"token": "value",
		},
	}
}

// TestSanitizeSecretStripsDataWhenRedactEnabled converts the former "stripping is always on"
// assertion into a test of the opt-in stripping path (redactSecrets=true): an operator who
// wants Secret values to never leave the cluster.
func TestSanitizeSecretStripsDataWhenRedactEnabled(t *testing.T) {
	secret := newTestSecret()

	SanitizeSecret(secret, true)

	if len(secret.Data) != 0 || len(secret.StringData) != 0 {
		t.Fatal("expected secret data fields to be cleared when redactSecrets is true")
	}
	if _, ok := secret.Annotations[lastAppliedConfigAnnotation]; ok {
		t.Fatal("expected last-applied-configuration annotation to be removed")
	}
	if secret.Annotations["other"] != "keep" {
		t.Fatalf("annotation other = %q, want keep", secret.Annotations["other"])
	}
	if len(secret.ManagedFields) != 0 {
		t.Fatal("expected managed fields to be cleared")
	}
	if secret.Type != corev1.SecretTypeOpaque {
		t.Fatalf("type = %q, want Opaque", secret.Type)
	}
}

// TestSanitizeSecretPreservesDataByDefault proves the new default behavior: with
// redactSecrets=false (the owner's chosen default), Secret values survive sanitization while
// metadata scrubbing (managedFields, last-applied-configuration) still happens unconditionally.
func TestSanitizeSecretPreservesDataByDefault(t *testing.T) {
	secret := newTestSecret()

	SanitizeSecret(secret, false)

	if string(secret.Data["password"]) != "secret" {
		t.Fatalf("Data[password] = %q, want secret to survive when redactSecrets is false", secret.Data["password"])
	}
	if secret.StringData["token"] != "value" {
		t.Fatalf("StringData[token] = %q, want value to survive when redactSecrets is false", secret.StringData["token"])
	}
	if _, ok := secret.Annotations[lastAppliedConfigAnnotation]; ok {
		t.Fatal("expected last-applied-configuration annotation to be removed regardless of redactSecrets")
	}
	if secret.Annotations["other"] != "keep" {
		t.Fatalf("annotation other = %q, want keep", secret.Annotations["other"])
	}
	if len(secret.ManagedFields) != 0 {
		t.Fatal("expected managed fields to be cleared regardless of redactSecrets")
	}
}

// TestSanitizeUnstructuredSecretsStripsDataWhenRedactEnabled covers the second code path
// (the unstructured/dynamic-client branch) with stripping opted in, so behavior does not
// depend on which client type the informer produced for a given resource.
func TestSanitizeUnstructuredSecretsStripsDataWhenRedactEnabled(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":          "db-creds",
			"namespace":     "default",
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
			"annotations": map[string]any{
				lastAppliedConfigAnnotation: `{"apiVersion":"v1"}`,
				"other":                     "keep",
			},
		},
		"data": map[string]any{"password": "c2VjcmV0"},
	}}

	SanitizeUnstructured(obj, "secrets", true)

	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "data"); found {
		t.Fatal("expected data field to be removed when redactSecrets is true")
	}
	metadata, _, _ := unstructured.NestedMap(obj.Object, "metadata")
	if _, ok := metadata["managedFields"]; ok {
		t.Fatal("expected managedFields to be removed")
	}
	ann, _ := metadata["annotations"].(map[string]any)
	if _, ok := ann[lastAppliedConfigAnnotation]; ok {
		t.Fatal("expected last-applied-configuration annotation to be removed")
	}
	if ann["other"] != "keep" {
		t.Fatalf("annotation other = %v, want keep", ann["other"])
	}
}

// TestSanitizeUnstructuredSecretsPreservesDataByDefault proves the default (redactSecrets=
// false) path preserves Secret data in the unstructured branch too, while metadata scrubbing
// still happens unconditionally.
func TestSanitizeUnstructuredSecretsPreservesDataByDefault(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":          "db-creds",
			"namespace":     "default",
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
			"annotations": map[string]any{
				lastAppliedConfigAnnotation: `{"apiVersion":"v1"}`,
			},
		},
		"data": map[string]any{"password": "c2VjcmV0"},
	}}

	SanitizeUnstructured(obj, "secrets", false)

	data, found, _ := unstructured.NestedMap(obj.Object, "data")
	if !found {
		t.Fatal("expected data field to survive when redactSecrets is false")
	}
	if data["password"] != "c2VjcmV0" {
		t.Fatalf("data[password] = %v, want c2VjcmV0 to survive when redactSecrets is false", data["password"])
	}
	metadata, _, _ := unstructured.NestedMap(obj.Object, "metadata")
	if _, ok := metadata["managedFields"]; ok {
		t.Fatal("expected managedFields to be removed regardless of redactSecrets")
	}
	if ann, ok := metadata["annotations"].(map[string]any); ok {
		if _, ok := ann[lastAppliedConfigAnnotation]; ok {
			t.Fatal("expected last-applied-configuration annotation to be removed regardless of redactSecrets")
		}
	}
}

// TestMarshalObjectJSONNoSecretPayloadWhenRedactEnabled converts the former unconditional
// assertion into a test of the opt-in stripping path end to end through MarshalObjectJSON.
func TestMarshalObjectJSONNoSecretPayloadWhenRedactEnabled(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("v")},
	}

	raw, err := MarshalObjectJSON(secret, "secrets", true)
	if err != nil {
		t.Fatalf("MarshalObjectJSON: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["data"]; ok {
		t.Fatal("json must not contain data field when redactSecrets is true")
	}
	if _, ok := decoded["stringData"]; ok {
		t.Fatal("json must not contain stringData field when redactSecrets is true")
	}
}

// TestMarshalObjectJSONPreservesSecretPayloadByDefault proves the new default behavior end
// to end: with redactSecrets=false, a Secret's data survives sanitization and JSON encoding,
// which is what allows the platform to serve Secret values in the resource explorer.
func TestMarshalObjectJSONPreservesSecretPayloadByDefault(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "x",
			Namespace: "ns",
			Annotations: map[string]string{
				lastAppliedConfigAnnotation: `{"apiVersion":"v1"}`,
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Data: map[string][]byte{"k": []byte("v")},
	}

	raw, err := MarshalObjectJSON(secret, "secrets", false)
	if err != nil {
		t.Fatalf("MarshalObjectJSON: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatal("json must contain data field when redactSecrets is false")
	}
	// Secret.Data is []byte, marshaled by encoding/json as standard base64.
	if data["k"] != "dg==" {
		t.Fatalf("data[k] = %v, want base64(v) = dg== to survive end to end", data["k"])
	}
	metadata, ok := decoded["metadata"].(map[string]any)
	if !ok {
		t.Fatal("json must contain metadata field")
	}
	if _, ok := metadata["managedFields"]; ok {
		t.Fatal("managedFields must be removed regardless of redactSecrets")
	}
	if ann, ok := metadata["annotations"].(map[string]any); ok {
		if _, ok := ann[lastAppliedConfigAnnotation]; ok {
			t.Fatal("last-applied-configuration annotation must be removed regardless of redactSecrets")
		}
	}
}
