package k8sresource

import (
	"testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func TestParseBuiltinAlias(t *testing.T) {
	d, err := Parse("jobs")
	if err != nil {
		t.Fatalf("Parse(jobs): %v", err)
	}
	if d.GVR.Resource != "jobs" || d.GVR.Group != "batch" {
		t.Fatalf("GVR = %+v", d.GVR)
	}
	if d.ProtoKind != agentv1.ResourceKind_RESOURCE_KIND_JOB {
		t.Fatalf("ProtoKind = %v", d.ProtoKind)
	}
}

func TestParseCoreShorthand(t *testing.T) {
	d, err := Parse("v1/pods")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.GVR.Group != "" || d.GVR.Version != "v1" || d.GVR.Resource != "pods" {
		t.Fatalf("GVR = %+v", d.GVR)
	}
}

func TestParseCRDGVR(t *testing.T) {
	d, err := Parse("monitoring.coreos.com/v1/prometheusrules")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.ProtoKind != agentv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED {
		t.Fatalf("ProtoKind = %v, want UNSPECIFIED", d.ProtoKind)
	}
	if d.APIVersion() != "monitoring.coreos.com/v1" {
		t.Fatalf("APIVersion = %q", d.APIVersion())
	}
	if d.MetricLabel != "prometheusrules" {
		t.Fatalf("MetricLabel = %q", d.MetricLabel)
	}
}

func TestParseSecretSkipAdded(t *testing.T) {
	d, err := Parse("secrets")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.SkipAdded {
		t.Fatal("secrets should skip added events")
	}
}
