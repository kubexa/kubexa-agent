package k8sresource_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
)

// grantedResources reads the resource names a slice of the ClusterRole
// template grants. Substring matching over the unrendered template, for the
// reason TestClusterRoleCoversRegistry documents.
func grantedResources(chart string) map[string]bool {
	granted := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*-\s+([a-z0-9./]+)\s*$`).FindAllStringSubmatch(chart, -1) {
		granted[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`resources:\s*\[([^\]]*)\]`).FindAllStringSubmatch(chart, -1) {
		for _, name := range strings.Split(m[1], ",") {
			granted[strings.Trim(strings.TrimSpace(name), `"'`)] = true
		}
	}
	return granted
}

// The chart's ClusterRole must cover every resource this registry knows.
//
// The registry is what the agent will accept in a query and what capability
// discovery advertises; the ClusterRole is what the API server will actually
// let the agent read. When the two drift, the failure is invisible here and
// only shows up on a deployed cluster as RBAC_DENIED for that one type, with
// nothing in the agent's own config to explain it.
//
// It has drifted once already: persistentvolumes, resourcequotas, limitranges
// and replicationcontrollers were in the registry from the start and were
// never in the ClusterRole (found 2026-08-02, before the 0.5.0 release).
//
// The check is a substring match over the rendered template rather than a YAML
// parse: the file is a Go template, its resource lists are gated behind
// {{- if }} blocks that only Helm can evaluate, and a resource name appearing
// anywhere in it is exactly the property that matters. Coarse, but it cannot
// pass while a name is genuinely absent, which is the only direction that hurts.
func TestClusterRoleCoversRegistry(t *testing.T) {
	path := filepath.Join("..", "..", "..", "helm", "kubexa-agent", "templates", "clusterrole.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	chart := string(raw)

	granted := grantedResources(chart)

	missing := []string{}
	seen := map[string]bool{}
	for _, alias := range k8sresource.KnownAliases() {
		d, err := k8sresource.Parse(alias)
		if err != nil {
			t.Fatalf("Parse(%q): %v", alias, err)
		}
		resource := d.GVR.Resource
		if seen[resource] {
			continue
		}
		seen[resource] = true
		if !granted[resource] {
			missing = append(missing, resource)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("ClusterRole does not grant read access to registry resources: %v\n"+
			"A deployed agent returns RBAC_DENIED for each of these while the agent's own "+
			"query policy reports them as allowed.", missing)
	}
}

// The live usage columns read metrics.k8s.io through the QUERY path, which an
// install may use with scraping turned off. Gating the RBAC rule on
// collect.metrics.enabled alone would make those reads RBAC_DENIED on exactly
// that install, with nothing in the agent's own config to explain it.
func TestClusterRoleGrantsMetricsForLiveQueriesToo(t *testing.T) {
	path := filepath.Join("..", "..", "..", "helm", "kubexa-agent", "templates", "clusterrole.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	chart := string(raw)

	idx := strings.Index(chart, `- apiGroups: ["metrics.k8s.io"]`)
	if idx < 0 {
		t.Fatal("no metrics.k8s.io rule in the ClusterRole")
	}
	// The {{- if }} immediately above the rule is the one that gates it.
	head := chart[:idx]
	gate := head[strings.LastIndex(head, "{{- if"):]
	if !strings.Contains(gate, "query.enabled") {
		t.Errorf("the metrics.k8s.io rule is not granted for live queries; gate is %q", strings.TrimSpace(gate))
	}
	if !strings.Contains(gate, "collect.metrics.enabled") {
		t.Errorf("the metrics.k8s.io rule no longer covers scraping; gate is %q", strings.TrimSpace(gate))
	}
}

// rbac.readAll grants apiGroups:["*"], resources:["*"] -- but it is OFF by
// default, so it must never be what satisfies the registry coverage check.
// Folding the enumerated rules into it, or letting the "*" entry count as
// covering a named resource, would leave every default install taking
// RBAC_DENIED while this file stayed green.
func TestReadAllDoesNotSatisfyRegistryCoverage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "helm", "kubexa-agent", "templates", "clusterrole.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	chart := string(raw)

	start := strings.Index(chart, "{{- if .Values.rbac.readAll }}")
	if start < 0 {
		t.Fatal("no rbac.readAll block in the ClusterRole")
	}
	end := strings.Index(chart[start:], "{{- end }}")
	if end < 0 {
		t.Fatal("the rbac.readAll block is not closed")
	}
	granted := grantedResources(chart[start : start+end])

	for _, alias := range k8sresource.KnownAliases() {
		d, err := k8sresource.Parse(alias)
		if err != nil {
			t.Fatalf("Parse(%q): %v", alias, err)
		}
		if granted[d.GVR.Resource] {
			t.Errorf("the rbac.readAll block names %q; the enumerated rules must be what covers the registry",
				d.GVR.Resource)
		}
	}
}
