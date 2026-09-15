package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ExecConfig governs interactive console sessions the Kubexa platform may
// open in this cluster. Like MutateConfig it is its own gate: nothing here
// inherits from query, collect or mutate, and every section defaults to off.
type ExecConfig struct {
	Pod  PodExecConfig  `yaml:"pod"`
	Node NodeExecConfig `yaml:"node"`
}

// PodExecConfig is the `exec.pod` section.
type PodExecConfig struct {
	// Enabled false refuses every exec_open. Unset means FALSE.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Rules lists which containers may be entered. No rule, no shell.
	Rules []PodExecRule `yaml:"rules,omitempty"`
	// DefaultShell runs when ExecOpen.command is empty. Default ["/bin/sh"].
	DefaultShell []string `yaml:"default_shell,omitempty"`
	// MaxSessionSec is the hard stop for one session. Default 1800, [10, 86400].
	MaxSessionSec int `yaml:"max_session_sec,omitempty"`
	// MaxSessions caps concurrent sessions on this agent. Default 4, [1, 64].
	MaxSessions int `yaml:"max_sessions,omitempty"`
	// ResumeWindowSec is how long a session is kept alive with no attached
	// stream, so a proxy cut resumes instead of killing the shell. Unset or
	// 0 means the default (60); a negative value means no resume (a dropped
	// stream ends the session); at most 600. See ExecPodSettings.
	ResumeWindowSec int `yaml:"resume_window_sec,omitempty"`
}

// PodExecRule permits a shell in a set of containers. Every empty field
// matches everything, the convention mutate.rules[].names already uses.
type PodExecRule struct {
	ID         string   `yaml:"id,omitempty"`
	Namespace  string   `yaml:"namespace,omitempty"`  // trailing "*" supported
	Names      []string `yaml:"names,omitempty"`      // pod names, trailing "*" supported
	Containers []string `yaml:"containers,omitempty"` // trailing "*" supported
}

// PodExecSettings is PodExecConfig with defaults applied. Callers read this,
// never the raw struct, so a zero field cannot leak into a limit.
type PodExecSettings struct {
	DefaultShell    []string
	MaxSessionSec   int
	MaxSessions     int
	ResumeWindowSec int
}

const (
	defaultExecMaxSessionSec   = 1800
	defaultExecMaxSessions     = 4
	defaultExecResumeWindowSec = 60
)

// NodeExecConfig is the `exec.node` section: a shell on the node itself,
// through a per-session privileged helper Pod the agent creates on that
// node and enters with nsenter. Off unless Enabled is true.
type NodeExecConfig struct {
	// Enabled false refuses every node exec_open. Unset means FALSE.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Nodes lists node-name patterns (trailing "*" supported). Empty
	// matches NOTHING and is a validation error when enabled: the operator
	// writes ["*"] to mean every node.
	Nodes []string `yaml:"nodes,omitempty"`
	// Image is the helper Pod's image. REQUIRED when enabled; there is no
	// default on purpose (see the design spec, §2.1): any image with
	// `sleep` and `nsenter` works, and the operator names one this cluster
	// can pull.
	Image string `yaml:"image,omitempty"`
	// Namespace is where helper Pods are created. Empty means the agent's
	// own namespace (POD_NAMESPACE), where an ownerReference to the agent
	// Pod is legal and garbage collection removes helpers with the agent.
	// Set to anything else and the ownerReference is dropped.
	Namespace string `yaml:"namespace,omitempty"`
	// Shell is what nsenter runs inside the host namespaces. Default
	// ["/bin/sh", "-l"].
	Shell []string `yaml:"shell,omitempty"`
	// MaxSessionSec is the hard stop for one session and, plus a minute,
	// the helper Pod's activeDeadlineSeconds. Default 1800, [10, 86400].
	MaxSessionSec int `yaml:"max_session_sec,omitempty"`
	// MaxSessions caps concurrent NODE sessions (separate from exec.pod's
	// cap). Default 1, [1, 16].
	MaxSessions int `yaml:"max_sessions,omitempty"`
	// HelperReadyTimeoutSec bounds the wait for the helper Pod to become
	// Running, image pull included. Default 60, [5, 600].
	HelperReadyTimeoutSec int `yaml:"helper_ready_timeout_sec,omitempty"`
}

// NodeExecSettings is NodeExecConfig with defaults applied. Callers read
// this, never the raw struct.
type NodeExecSettings struct {
	Image                 string
	Namespace             string
	Shell                 []string
	MaxSessionSec         int
	MaxSessions           int
	HelperReadyTimeoutSec int
}

const (
	defaultNodeExecMaxSessionSec         = 1800
	defaultNodeExecMaxSessions           = 1
	defaultNodeExecHelperReadyTimeoutSec = 60
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ExecPodEnabled reports whether any pod console is answered at all.
func (c *Config) ExecPodEnabled() bool {
	if c == nil || c.Exec.Pod.Enabled == nil {
		return false
	}
	return *c.Exec.Pod.Enabled
}

// ExecPodRules returns the configured rules. There is no inheritance.
func (c *Config) ExecPodRules() []PodExecRule {
	if c == nil {
		return nil
	}
	return c.Exec.Pod.Rules
}

// ExecPodSettings applies defaults. It does not validate; Validate does.
func (c *Config) ExecPodSettings() PodExecSettings {
	s := PodExecSettings{
		DefaultShell:    []string{"/bin/sh"},
		MaxSessionSec:   defaultExecMaxSessionSec,
		MaxSessions:     defaultExecMaxSessions,
		ResumeWindowSec: defaultExecResumeWindowSec,
	}
	if c == nil {
		return s
	}
	p := c.Exec.Pod
	if len(p.DefaultShell) > 0 {
		s.DefaultShell = append([]string(nil), p.DefaultShell...)
	}
	if p.MaxSessionSec != 0 {
		s.MaxSessionSec = p.MaxSessionSec
	}
	if p.MaxSessions != 0 {
		s.MaxSessions = p.MaxSessions
	}
	// ResumeWindowSec: 0 is a legitimate value ("no resume"), so the default
	// applies only when the key is absent -- which yaml cannot distinguish
	// from 0 on an int. Operators who want no resume write a negative
	// number; it is clamped to 0 below and validation permits it.
	if p.ResumeWindowSec < 0 {
		s.ResumeWindowSec = 0
	} else if p.ResumeWindowSec > 0 {
		s.ResumeWindowSec = p.ResumeWindowSec
	}
	return s
}

// ExecNodeEnabled reports whether any node console is answered at all.
func (c *Config) ExecNodeEnabled() bool {
	if c == nil || c.Exec.Node.Enabled == nil {
		return false
	}
	return *c.Exec.Node.Enabled
}

// ExecNodeRules returns the configured node-name patterns. No inheritance.
func (c *Config) ExecNodeRules() []string {
	if c == nil {
		return nil
	}
	return c.Exec.Node.Nodes
}

// ExecNodeSettings applies defaults. It does not validate; Validate does.
func (c *Config) ExecNodeSettings() NodeExecSettings {
	s := NodeExecSettings{
		Shell:                 []string{"/bin/sh", "-l"},
		MaxSessionSec:         defaultNodeExecMaxSessionSec,
		MaxSessions:           defaultNodeExecMaxSessions,
		HelperReadyTimeoutSec: defaultNodeExecHelperReadyTimeoutSec,
	}
	if c == nil {
		return s
	}
	n := c.Exec.Node
	s.Image = strings.TrimSpace(n.Image)
	s.Namespace = strings.TrimSpace(n.Namespace)
	if len(n.Shell) > 0 {
		s.Shell = append([]string(nil), n.Shell...)
	}
	if n.MaxSessionSec != 0 {
		s.MaxSessionSec = n.MaxSessionSec
	}
	if n.MaxSessions != 0 {
		s.MaxSessions = n.MaxSessions
	}
	if n.HelperReadyTimeoutSec != 0 {
		s.HelperReadyTimeoutSec = n.HelperReadyTimeoutSec
	}
	return s
}

// ValidateNodeExecRules validates the node-name patterns in isolation and
// does NOT consult Enabled, for the reason ValidatePodExecRules gives.
func ValidateNodeExecRules(patterns []string) []string {
	var errs []string
	for _, p := range patterns {
		if err := validatePattern(p); err != nil {
			errs = append(errs, fmt.Sprintf("exec.node.nodes: %q: %v", p, err))
		}
	}
	return errs
}

func (c *Config) validateExecNode() []string {
	if c == nil || !c.ExecNodeEnabled() {
		return nil
	}
	var errs []string
	n := c.Exec.Node
	if strings.TrimSpace(n.Image) == "" {
		errs = append(errs, "exec.node.image is required when exec.node.enabled is true")
	}
	if len(n.Nodes) == 0 {
		errs = append(errs, `exec.node.nodes must name at least one pattern (["*"] for every node)`)
	}
	errs = append(errs, ValidateNodeExecRules(n.Nodes)...)
	if ns := strings.TrimSpace(n.Namespace); ns != "" && !dns1123Label.MatchString(ns) {
		errs = append(errs, "exec.node.namespace must be a lowercase DNS-1123 label")
	}
	for i, arg := range n.Shell {
		if strings.TrimSpace(arg) == "" {
			errs = append(errs, fmt.Sprintf("exec.node.shell[%d] must not be empty", i))
		}
	}
	if n.MaxSessionSec != 0 && (n.MaxSessionSec < 10 || n.MaxSessionSec > 86400) {
		errs = append(errs, "exec.node.max_session_sec must be between 10 and 86400")
	}
	if n.MaxSessions < 0 || n.MaxSessions > 16 {
		errs = append(errs, "exec.node.max_sessions must be between 1 and 16")
	}
	if n.HelperReadyTimeoutSec != 0 && (n.HelperReadyTimeoutSec < 5 || n.HelperReadyTimeoutSec > 600) {
		errs = append(errs, "exec.node.helper_ready_timeout_sec must be between 5 and 600")
	}
	// The session clock starts at open and the helper wait runs under it
	// (internal/exec: prepare runs inside Session.run). A max_session
	// shorter than the helper wait cannot ever reach a prompt on an
	// uncached pull -- it ends as MAX_SESSION under a "starting helper"
	// line. Compared on the effective values so a default on either side
	// counts.
	if eff := c.ExecNodeSettings(); eff.MaxSessionSec < eff.HelperReadyTimeoutSec {
		errs = append(errs, fmt.Sprintf("exec.node.max_session_sec (%d) must be at least exec.node.helper_ready_timeout_sec (%d)",
			eff.MaxSessionSec, eff.HelperReadyTimeoutSec))
	}
	return errs
}

// validateExecPod binds while exec.pod is on OR exec.node is on: a node
// console shares exec.pod's resume_window_sec and its client set, so a
// disabled-but-invalid exec.pod would otherwise surface only at boot, as
// exec.New's "policy is required" (cmd/agent/main.go names the section
// there as a second net).
func (c *Config) validateExecPod() []string {
	if c == nil || (!c.ExecPodEnabled() && !c.ExecNodeEnabled()) {
		return nil
	}
	var errs []string
	errs = append(errs, ValidatePodExecRules(c.Exec.Pod.Rules)...)
	p := c.Exec.Pod
	for i, arg := range p.DefaultShell {
		if strings.TrimSpace(arg) == "" {
			errs = append(errs, fmt.Sprintf("exec.pod.default_shell[%d] must not be empty", i))
		}
	}
	if p.MaxSessionSec != 0 && (p.MaxSessionSec < 10 || p.MaxSessionSec > 86400) {
		errs = append(errs, "exec.pod.max_session_sec must be between 10 and 86400")
	}
	if p.MaxSessions < 0 || p.MaxSessions > 64 {
		errs = append(errs, "exec.pod.max_sessions must be between 1 and 64")
	}
	if p.ResumeWindowSec > 600 {
		errs = append(errs, "exec.pod.resume_window_sec must be at most 600")
	}
	return errs
}

func (c *Config) validateExec() []string {
	if c == nil {
		return nil
	}
	errs := c.validateExecPod()
	return append(errs, c.validateExecNode()...)
}

// ValidatePodExecRules validates each rule in isolation and does NOT consult
// Enabled, for the reason ValidateMutateRules gives: internal/exec/policy
// compiles rules unconditionally so a section enabled tomorrow was checked
// today.
func ValidatePodExecRules(rules []PodExecRule) []string {
	var errs []string
	for i, r := range rules {
		prefix := fmt.Sprintf("exec.pod.rules[%d]", i)
		if r.Namespace != "" {
			if err := validatePattern(r.Namespace); err != nil {
				errs = append(errs, fmt.Sprintf("%s.namespace: %v", prefix, err))
			}
		}
		for _, n := range r.Names {
			if err := validatePattern(n); err != nil {
				errs = append(errs, fmt.Sprintf("%s.names: %q: %v", prefix, n, err))
			}
		}
		for _, n := range r.Containers {
			if err := validatePattern(n); err != nil {
				errs = append(errs, fmt.Sprintf("%s.containers: %q: %v", prefix, n, err))
			}
		}
	}
	return errs
}
