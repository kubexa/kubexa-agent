package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

// blockProperties walks the generated document down to one managed block's
// "properties" object, failing the test (rather than panicking) if any step
// of the path is missing or the wrong shape.
func blockProperties(t *testing.T, schema map[string]any, block string) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no top-level \"properties\"")
	}
	blockSchema, ok := props[block].(map[string]any)
	if !ok {
		t.Fatalf("schema has no block %q", block)
	}
	blockProps, ok := blockSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("block %q has no \"properties\"", block)
	}
	return blockProps
}

// TestSchemaCoversExactlyTheAllowedKeys pins the generated schema to
// managedBlockAllowedKeys (pkg/config/blocks.go) as it stands today: a
// property for every allowed key of every managed block, and none that are
// not allowed. redact_secrets is named explicitly because it is the one
// whose presence would be a security-relevant lie -- it would tell a
// cluster owner the platform can switch their Secret redaction off, which
// it cannot (Ruling B).
func TestSchemaCoversExactlyTheAllowedKeys(t *testing.T) {
	schema, err := buildSchema()
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}

	want := map[string][]string{
		"collect.logs":    {"enabled", "follow", "tail_lines", "exclude_namespaces", "rules"},
		"collect.state":   {"enabled", "resync_period", "rules"},
		"collect.metrics": {"enabled", "pod_interval", "node_interval", "rules"},
		"query":           {"enabled", "rules"},
		"buffer":          {"batch_size"},
	}

	for block, wantKeys := range want {
		props := blockProperties(t, schema, block)
		got := make([]string, 0, len(props))
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		wantSorted := append([]string(nil), wantKeys...)
		sort.Strings(wantSorted)
		if !reflect.DeepEqual(got, wantSorted) {
			t.Errorf("%s: properties = %v, want %v", block, got, wantSorted)
		}
	}

	if _, ok := blockProperties(t, schema, "collect.state")["redact_secrets"]; ok {
		t.Error("collect.state schema exposes redact_secrets")
	}
	if _, ok := blockProperties(t, schema, "query")["redact_secrets"]; ok {
		t.Error("query schema exposes redact_secrets")
	}
	for _, key := range []string{"custom_endpoints", "cadvisor", "kube_state_metrics", "kube_metrics"} {
		if _, ok := blockProperties(t, schema, "collect.metrics")[key]; ok {
			t.Errorf("collect.metrics schema exposes %q", key)
		}
	}
	for _, key := range []string{"spill_dir", "max_disk_bytes", "max_memory_bytes", "flush_interval"} {
		if _, ok := blockProperties(t, schema, "buffer")[key]; ok {
			t.Errorf("buffer schema exposes %q", key)
		}
	}
}

// TestSchemaUsesYAMLSpelling pins that property names come from the yaml
// tag, not encoding/json's default field-name casing. The structs carry no
// json tags, so anything reaching for encoding/json's defaults would emit
// "PodNames" instead of "pod_names" and the app's form fields would
// silently bind to nothing.
func TestSchemaUsesYAMLSpelling(t *testing.T) {
	schema, err := buildSchema()
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	rulesSchema, ok := blockProperties(t, schema, "collect.logs")["rules"].(map[string]any)
	if !ok {
		t.Fatalf("collect.logs has no \"rules\" property")
	}
	items, ok := rulesSchema["items"].(map[string]any)
	if !ok {
		t.Fatalf("collect.logs.rules has no \"items\"")
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("collect.logs.rules items have no \"properties\"")
	}
	if _, ok := itemProps["pod_names"]; !ok {
		t.Error("expected the yaml-spelled \"pod_names\" among rule properties")
	}
	if _, ok := itemProps["PodNames"]; ok {
		t.Error("schema used the Go field name \"PodNames\" instead of its yaml tag")
	}
	if _, ok := itemProps["label_selector"]; !ok {
		t.Error("expected the yaml-spelled \"label_selector\" among rule properties")
	}
}

// TestAnAllowlistKeyWithNoStructFieldIsAnError exercises schemaForAllowedKeys
// directly: an allowlist key with no matching yaml tag on the struct is a
// hard error, not a silently skipped property. This can't happen with the
// real allowlist today (blocks_internal_test.go pins that), so this test
// supplies a synthetic mismatch to prove the generator itself would refuse
// to paper over one if it ever occurred.
func TestAnAllowlistKeyWithNoStructFieldIsAnError(t *testing.T) {
	_, err := schemaForAllowedKeys([]string{"enabled", "bogus_key"}, reflect.TypeOf(config.LogsCollectConfig{}))
	if err == nil {
		t.Fatal("expected an error for an allowlist key with no matching struct field")
	}
	if !strings.Contains(err.Error(), "bogus_key") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

// TestSchemaCarriesNoVersionField pins Ruling A: the generated document
// carries nothing version-shaped. git describe changes on every commit, so
// a baked-in version would make the CI diff step fail on every merge -- a
// check that blocks everything while proving nothing. Version skew is the
// agent's job to reject and report, not the schema's job to declare.
func TestSchemaCarriesNoVersionField(t *testing.T) {
	schema, err := buildSchema()
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "version") {
		t.Errorf("generated schema mentions \"version\": %s", raw)
	}
}
