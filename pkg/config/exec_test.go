package config_test

import (
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

func TestExecPodDefaultsOff(t *testing.T) {
	var c config.Config
	if c.ExecPodEnabled() {
		t.Fatal("exec.pod must default to disabled")
	}
	s := c.ExecPodSettings()
	if got := strings.Join(s.DefaultShell, " "); got != "/bin/sh" {
		t.Fatalf("default_shell default = %q, want /bin/sh", got)
	}
	if s.MaxSessionSec != 1800 || s.MaxSessions != 4 || s.ResumeWindowSec != 60 {
		t.Fatalf("defaults = %+v", s)
	}
}

func TestExecPodInheritsNothing(t *testing.T) {
	c := config.Config{}
	c.Query.Enabled = boolPtr(true)
	c.Mutate.Enabled = boolPtr(true)
	if c.ExecPodEnabled() {
		t.Fatal("exec.pod must not inherit enabled from query or mutate")
	}
}

func TestValidatePodExecRules(t *testing.T) {
	cases := []struct {
		name string
		rule config.PodExecRule
		want string // substring of the violation, "" for valid
	}{
		{"empty rule matches everything", config.PodExecRule{}, ""},
		{"namespace glob", config.PodExecRule{Namespace: "dev-*"}, ""},
		{"bad namespace pattern", config.PodExecRule{Namespace: "de*v"}, "namespace"},
		{"bad name pattern", config.PodExecRule{Names: []string{"a*b"}}, "names"},
		{"bad container pattern", config.PodExecRule{Containers: []string{"*sidecar"}}, "containers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := config.ValidatePodExecRules([]config.PodExecRule{tc.rule})
			if tc.want == "" && len(got) != 0 {
				t.Fatalf("unexpected violations: %v", got)
			}
			if tc.want != "" && (len(got) == 0 || !strings.Contains(got[0], tc.want)) {
				t.Fatalf("want a violation mentioning %q, got %v", tc.want, got)
			}
		})
	}
}

func TestExecPodSettingsAreBounded(t *testing.T) {
	c := config.Config{}
	c.Exec.Pod.Enabled = boolPtr(true)
	c.Exec.Pod.MaxSessionSec = 5
	c.Exec.Pod.MaxSessions = 65
	c.Exec.Pod.ResumeWindowSec = 601
	c.Exec.Pod.DefaultShell = []string{""}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected violations")
	}
	for _, want := range []string{"max_session_sec", "max_sessions", "resume_window_sec", "default_shell"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("violations do not mention %s: %v", want, err)
		}
	}
}

func TestExecPodDisabledSkipsBoundsAtLoad(t *testing.T) {
	c := config.Config{}
	c.Exec.Pod.MaxSessionSec = 5 // invalid, but the section is off
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "max_session_sec") {
		t.Fatalf("a disabled section must not fail load on its bounds: %v", err)
	}
}
