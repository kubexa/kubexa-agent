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

// A node console shares exec.pod's resume_window_sec and its client set,
// so exec.pod's scalars bind while exec.node is on even with exec.pod off.
func TestExecNodeOnValidatesExecPodScalars(t *testing.T) {
	on := true
	c := config.Config{}
	c.Exec.Node = config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}}
	c.Exec.Pod.ResumeWindowSec = 601
	c.Exec.Pod.MaxSessionSec = 5
	err := c.Validate()
	if err == nil {
		t.Fatal("expected violations")
	}
	for _, want := range []string{"exec.pod.resume_window_sec", "exec.pod.max_session_sec"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("violations do not mention %s: %v", want, err)
		}
	}
}

func TestExecNodeDefaultsAndAccessors(t *testing.T) {
	var nilCfg *config.Config
	if nilCfg.ExecNodeEnabled() {
		t.Fatal("nil config must not enable the node console")
	}
	c := &config.Config{}
	if c.ExecNodeEnabled() {
		t.Fatal("unset enabled must be false")
	}
	s := c.ExecNodeSettings()
	if s.MaxSessionSec != 1800 || s.MaxSessions != 1 || s.HelperReadyTimeoutSec != 60 {
		t.Fatalf("defaults = %+v", s)
	}
	if len(s.Shell) != 2 || s.Shell[0] != "/bin/sh" || s.Shell[1] != "-l" {
		t.Fatalf("default shell = %v", s.Shell)
	}
	on := true
	c.Exec.Node = config.NodeExecConfig{Enabled: &on, Nodes: []string{"aks-*"}, Image: "busybox:1.36",
		Namespace: "shells", Shell: []string{"/bin/bash"}, MaxSessionSec: 60, MaxSessions: 2, HelperReadyTimeoutSec: 5}
	if !c.ExecNodeEnabled() {
		t.Fatal("enabled")
	}
	if got := c.ExecNodeRules(); len(got) != 1 || got[0] != "aks-*" {
		t.Fatalf("rules = %v", got)
	}
	s = c.ExecNodeSettings()
	if s.Image != "busybox:1.36" || s.Namespace != "shells" || s.MaxSessionSec != 60 || s.MaxSessions != 2 ||
		s.HelperReadyTimeoutSec != 5 || len(s.Shell) != 1 || s.Shell[0] != "/bin/bash" {
		t.Fatalf("settings = %+v", s)
	}
}

func TestExecNodeValidation(t *testing.T) {
	on := true
	cases := []struct {
		name string
		node config.NodeExecConfig
		want string // substring of one violation; "" means valid
	}{
		{"disabled needs nothing", config.NodeExecConfig{}, ""},
		{"enabled without image", config.NodeExecConfig{Enabled: &on, Nodes: []string{"*"}}, "exec.node.image is required"},
		{"enabled without nodes", config.NodeExecConfig{Enabled: &on, Image: "busybox"}, "exec.node.nodes must name at least one pattern"},
		{"bad pattern", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"a*b"}}, "exec.node.nodes"},
		{"empty shell arg", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, Shell: []string{" "}}, "exec.node.shell[0] must not be empty"},
		{"max_session_sec low", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, MaxSessionSec: 5}, "exec.node.max_session_sec"},
		{"max_sessions high", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, MaxSessions: 17}, "exec.node.max_sessions"},
		{"ready timeout high", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, HelperReadyTimeoutSec: 601}, "exec.node.helper_ready_timeout_sec"},
		{"bad namespace", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, Namespace: "Not_Valid"}, "exec.node.namespace"},
		// The session clock starts at open and the helper wait runs under it:
		// a max_session shorter than the helper wait ends every uncached
		// pull as MAX_SESSION under a "starting helper" line.
		{"max_session below ready timeout", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, MaxSessionSec: 60, HelperReadyTimeoutSec: 120}, "exec.node.max_session_sec (60) must be at least exec.node.helper_ready_timeout_sec (120)"},
		{"max_session below the default ready timeout", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, MaxSessionSec: 30}, "exec.node.max_session_sec (30) must be at least exec.node.helper_ready_timeout_sec (60)"},
		{"max_session equal to ready timeout", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}, MaxSessionSec: 120, HelperReadyTimeoutSec: 120}, ""},
		{"valid", config.NodeExecConfig{Enabled: &on, Image: "busybox", Nodes: []string{"*"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{}
			c.Exec.Node = tc.node
			got := config.ValidateExecForTest(c)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected violations: %v", got)
				}
				return
			}
			for _, v := range got {
				if strings.Contains(v, tc.want) {
					return
				}
			}
			t.Fatalf("violations %v lack %q", got, tc.want)
		})
	}
}

// Rules are validated even when disabled, the ruling every other section
// follows: a section switched on tomorrow was checked today.
func TestValidateNodeExecRulesIgnoresEnabled(t *testing.T) {
	if v := config.ValidateNodeExecRules([]string{"ok-*", "*bad"}); len(v) != 1 || !strings.Contains(v[0], "*bad") {
		t.Fatalf("violations = %v", v)
	}
}
