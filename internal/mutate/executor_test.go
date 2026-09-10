package mutate_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/mutate"
	"github.com/kubexa/kubexa-agent/internal/mutate/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

var deployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func deployment(ns, name, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "resourceVersion": rv,
		},
		"spec": map[string]any{
			"replicas": int64(1),
			"template": map[string]any{"metadata": map[string]any{}},
		},
	}}
}

func newFakeDynamic(objs []runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{deployGVR: "DeploymentList"}, objs...)
}

func newExecutorWithClient(
	t *testing.T,
	dyn *dynamicfake.FakeDynamicClient,
	rules ...config.MutateRule,
) *mutate.Executor {
	t.Helper()
	enabled := true
	p, err := policy.Compile(&config.Config{
		Mutate: config.MutateConfig{Enabled: &enabled, Rules: rules},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	e, err := mutate.New(mutate.Options{Clients: k8s.QueryClients{Dynamic: dyn}, Policy: p})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return e
}

func newExecutor(t *testing.T, objs []runtime.Object, rules ...config.MutateRule) *mutate.Executor {
	t.Helper()
	return newExecutorWithClient(t, newFakeDynamic(objs), rules...)
}

func req(verb agentv1.MutationVerb, ns, name string) *agentv1.MutationRequest {
	return &agentv1.MutationRequest{
		MutationId: "m1",
		Ref:        &agentv1.ResourceRef{Group: "apps", Version: "v1", Resource: "deployments"},
		Verb:       verb,
		Namespace:  ns,
		Name:       name,
	}
}

// A policy refusal is reported INSIDE the result, never as a transport
// failure, and it carries POLICY_DENIED rather than RBAC_DENIED so the
// operator is sent to the config file rather than to the ClusterRole.
func TestPolicyDenialIsReportedInTheResult(t *testing.T) {
	e := newExecutor(t, nil, config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"patch"}})
	res := e.Execute(context.Background(), req(agentv1.MutationVerb_MUTATION_VERB_DELETE, "dev", "web"))
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_POLICY_DENIED {
		t.Fatalf("code = %v, want POLICY_DENIED", res.GetError().GetCode())
	}
	if res.GetMutationId() != "m1" {
		t.Fatalf("mutation_id = %q, want m1", res.GetMutationId())
	}
}

// There is no blind-write path.
func TestPatchWithoutResourceVersionIsRefused(t *testing.T) {
	e := newExecutor(t, []runtime.Object{deployment("dev", "web", "7")},
		config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"patch"}})
	r := req(agentv1.MutationVerb_MUTATION_VERB_PATCH, "dev", "web")
	r.PatchType = "replace"
	r.Payload, _ = json.Marshal(deployment("dev", "web", "").Object)
	res := e.Execute(context.Background(), r)
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_INVALID {
		t.Fatalf("code = %v, want INVALID", res.GetError().GetCode())
	}
}

// TestStaleResourceVersionIsAConflict deliberately does NOT seed an object
// with resourceVersion "7" and submit "3" and expect the fake to refuse the
// write on its own: k8s.io/client-go/dynamic/fake's ObjectTracker does not
// enforce resourceVersion (or UID) preconditions on Update -- it applies
// whatever object it is given, stale or not. A test built that way would
// pass whether or not the executor ever set the precondition at all, i.e.
// it would assert nothing about the code under test.
//
// So this test is scoped to what a unit test of this package CAN prove: our
// own mapping from a 409 the API server returned to MUTATION_ERROR_CONFLICT.
// It installs a reactor that returns apierrors.NewConflict for the Update
// call the "replace" patch path issues, and asserts the result carries
// CONFLICT. The real precondition behaviour -- that the API server actually
// rejects a stale resourceVersion -- is unverifiable against this fake and
// is covered by the live acceptance run instead (see the plan's Task 16).
func TestStaleResourceVersionIsAConflict(t *testing.T) {
	dyn := newFakeDynamic([]runtime.Object{deployment("dev", "web", "7")})
	dyn.PrependReactor("update", "deployments",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "apps", Resource: "deployments"},
				"web", errors.New("the object has been modified"))
		})
	e := newExecutorWithClient(t, dyn,
		config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"patch"}})

	r := req(agentv1.MutationVerb_MUTATION_VERB_PATCH, "dev", "web")
	r.PatchType = "replace"
	r.ResourceVersion = "3"
	r.Payload, _ = json.Marshal(deployment("dev", "web", "3").Object)
	res := e.Execute(context.Background(), r)
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_CONFLICT {
		t.Fatalf("code = %v, want CONFLICT", res.GetError().GetCode())
	}
}

// RESTART is a patch of one annotation, using kubectl's own key so both
// tools agree about what a rollout restart is.
//
// This test does NOT let the fake apply the strategic merge patch itself.
// k8s.io/client-go/dynamic/fake's ObjectTracker runs a real
// strategicpatch.StrategicMergePatch, which derives its merge metadata from
// Go struct tags (patchStrategy/patchMergeKey) via reflection --
// unstructured.Unstructured is a map with no such tags, so every strategic
// merge patch against one fails in the fake with "unable to find api field
// in struct Unstructured for the json field ...". That is a limitation of
// the fake, not a real-cluster defect: a genuine API server strategic-merges
// a Deployment from its own compiled-in OpenAPI schema, which this
// client-side fake never has access to for an unstructured target. So this
// test installs a reactor that inspects the RAW patch bytes the executor
// sent -- proving OUR construction of the annotation is correct -- and hands
// back a manually merged object so the redaction/marshal half of the
// pipeline is exercised too. The real end-to-end application of a strategic
// merge patch against a live API server is covered by the acceptance run,
// not a unit test.
func TestRestartSetsKubectlsAnnotation(t *testing.T) {
	dyn := newFakeDynamic([]runtime.Object{deployment("dev", "web", "7")})
	var sentPatch []byte
	dyn.PrependReactor("patch", "deployments",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			pa, ok := action.(clienttesting.PatchAction)
			if !ok {
				return false, nil, nil
			}
			sentPatch = pa.GetPatch()
			var body map[string]any
			if err := json.Unmarshal(sentPatch, &body); err != nil {
				return true, nil, err
			}
			ann, _, _ := unstructured.NestedStringMap(body, "spec", "template", "metadata", "annotations")
			merged := deployment("dev", "web", "8")
			if err := unstructured.SetNestedStringMap(merged.Object, ann,
				"spec", "template", "metadata", "annotations"); err != nil {
				return true, nil, err
			}
			return true, merged, nil
		})
	e := newExecutorWithClient(t, dyn,
		config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"restart"}})

	res := e.Execute(context.Background(), req(agentv1.MutationVerb_MUTATION_VERB_RESTART, "dev", "web"))
	if res.GetError() != nil {
		t.Fatalf("unexpected error: %v", res.GetError())
	}
	if len(sentPatch) == 0 {
		t.Fatal("expected the executor to send a patch")
	}
	var sent map[string]any
	if err := json.Unmarshal(sentPatch, &sent); err != nil {
		t.Fatalf("patch body: %v", err)
	}
	sentAnn, _, _ := unstructured.NestedStringMap(sent, "spec", "template", "metadata", "annotations")
	if sentAnn["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Fatal("restart must set kubectl.kubernetes.io/restartedAt")
	}

	var obj map[string]any
	if err := json.Unmarshal(res.GetPayload(), &obj); err != nil {
		t.Fatalf("payload: %v", err)
	}
	respAnn, _, _ := unstructured.NestedStringMap(obj, "spec", "template", "metadata", "annotations")
	if respAnn["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Fatal("result payload must carry the restartedAt annotation")
	}
}

// A Pod cannot be restarted. Saying "restart" and doing a delete would be a
// different operation under the same word.
func TestRestartOnAnUnsupportedKindIsInvalid(t *testing.T) {
	e := newExecutor(t, nil, config.MutateRule{Resources: []string{"pods"}, Verbs: []string{"restart"}})
	r := req(agentv1.MutationVerb_MUTATION_VERB_RESTART, "dev", "web")
	r.Ref = &agentv1.ResourceRef{Version: "v1", Resource: "pods"}
	res := e.Execute(context.Background(), r)
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_INVALID {
		t.Fatalf("code = %v, want INVALID", res.GetError().GetCode())
	}
}

// CREATE takes its name from the body; a body whose namespace disagrees with
// the request is refused rather than silently preferring one of them.
func TestCreateRefusesANamespaceMismatch(t *testing.T) {
	e := newExecutor(t, nil, config.MutateRule{
		Namespace: "dev", Resources: []string{"deployments"}, Verbs: []string{"create"}})
	r := req(agentv1.MutationVerb_MUTATION_VERB_CREATE, "dev", "")
	r.Payload, _ = json.Marshal(deployment("prod", "web", "").Object)
	res := e.Execute(context.Background(), r)
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_INVALID {
		t.Fatalf("code = %v, want INVALID", res.GetError().GetCode())
	}
}

// Execute never returns an error and always copies MutationId, even for a
// nil request.
func TestNilRequestIsHandledWithoutMutationId(t *testing.T) {
	e := newExecutor(t, nil, config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"patch"}})
	res := e.Execute(context.Background(), nil)
	if res == nil {
		t.Fatal("Execute must never return a nil result")
	}
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_INVALID {
		t.Fatalf("code = %v, want INVALID", res.GetError().GetCode())
	}
	if res.GetMutationId() != "" {
		t.Fatalf("mutation_id = %q, want empty", res.GetMutationId())
	}
}

// A successful PATCH (replace, with a matching resourceVersion) actually
// updates the object and returns its new state.
func TestPatchReplaceAppliesTheUpdate(t *testing.T) {
	e := newExecutor(t, []runtime.Object{deployment("dev", "web", "7")},
		config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"patch"}})
	r := req(agentv1.MutationVerb_MUTATION_VERB_PATCH, "dev", "web")
	r.PatchType = "replace"
	r.ResourceVersion = "7"
	updated := deployment("dev", "web", "7")
	if err := unstructured.SetNestedField(updated.Object, int64(3), "spec", "replicas"); err != nil {
		t.Fatalf("set replicas: %v", err)
	}
	r.Payload, _ = json.Marshal(updated.Object)

	res := e.Execute(context.Background(), r)
	if res.GetError() != nil {
		t.Fatalf("unexpected error: %v", res.GetError())
	}
	var obj map[string]any
	if err := json.Unmarshal(res.GetPayload(), &obj); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// unstructured.NestedInt64 requires the stored value to already be a Go
	// int64; a plain json.Unmarshal into map[string]any decodes every JSON
	// number as float64, so NestedInt64 would silently report "not found"
	// here regardless of what the executor sent. NestedFloat64 matches what
	// encoding/json actually produces.
	replicas, _, _ := unstructured.NestedFloat64(obj, "spec", "replicas")
	if replicas != 3 {
		t.Fatalf("replicas = %v, want 3", replicas)
	}
}

// DELETE succeeds against a fake-tracked object and the result carries no
// payload.
func TestDeleteSucceedsWithEmptyPayload(t *testing.T) {
	e := newExecutor(t, []runtime.Object{deployment("dev", "web", "7")},
		config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"delete"}})
	res := e.Execute(context.Background(), req(agentv1.MutationVerb_MUTATION_VERB_DELETE, "dev", "web"))
	if res.GetError() != nil {
		t.Fatalf("unexpected error: %v", res.GetError())
	}
	if len(res.GetPayload()) != 0 {
		t.Fatalf("payload = %q, want empty", res.GetPayload())
	}
}

// CREATE succeeds and the created object is returned.
func TestCreateSucceeds(t *testing.T) {
	e := newExecutor(t, nil, config.MutateRule{
		Namespace: "dev", Resources: []string{"deployments"}, Verbs: []string{"create"}})
	r := req(agentv1.MutationVerb_MUTATION_VERB_CREATE, "dev", "")
	r.Payload, _ = json.Marshal(deployment("dev", "web", "").Object)
	res := e.Execute(context.Background(), r)
	if res.GetError() != nil {
		t.Fatalf("unexpected error: %v", res.GetError())
	}
	var obj map[string]any
	if err := json.Unmarshal(res.GetPayload(), &obj); err != nil {
		t.Fatalf("payload: %v", err)
	}
	name, _, _ := unstructured.NestedString(obj, "metadata", "name")
	if name != "web" {
		t.Fatalf("name = %q, want web", name)
	}
}

// A NotFound from the API server maps to MUTATION_ERROR_NOT_FOUND.
func TestDeleteOfMissingObjectIsNotFound(t *testing.T) {
	e := newExecutor(t, nil, config.MutateRule{Resources: []string{"deployments"}, Verbs: []string{"delete"}})
	res := e.Execute(context.Background(), req(agentv1.MutationVerb_MUTATION_VERB_DELETE, "dev", "missing"))
	if res.GetError().GetCode() != agentv1.MutationErrorCode_MUTATION_ERROR_NOT_FOUND {
		t.Fatalf("code = %v, want NOT_FOUND", res.GetError().GetCode())
	}
}
