package state

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSanitizeSecretStripsData(t *testing.T) {
	secret := &corev1.Secret{
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

	SanitizeSecret(secret)

	if len(secret.Data) != 0 || len(secret.StringData) != 0 {
		t.Fatal("expected secret data fields to be cleared")
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

func TestMarshalObjectJSONNoSecretPayload(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("v")},
	}

	raw, err := MarshalObjectJSON(secret, "secrets")
	if err != nil {
		t.Fatalf("MarshalObjectJSON: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["data"]; ok {
		t.Fatal("json must not contain data field")
	}
	if _, ok := decoded["stringData"]; ok {
		t.Fatal("json must not contain stringData field")
	}
}
