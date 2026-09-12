package config

import (
	"fmt"
	"strings"
)

// ExecConfig governs interactive console sessions the Kubexa platform may
// open in this cluster. Like MutateConfig it is its own gate: nothing here
// inherits from query, collect or mutate, and every section defaults to off.
type ExecConfig struct {
	Pod PodExecConfig `yaml:"pod"`
	// Node is phase C. There is no field yet: a phase-C `exec.node` key on a
	// phase-B agent surfaces as an advisory UnknownKeys warning at load, not
	// an error, and is not read.
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

func (c *Config) validateExec() []string {
	if c == nil || !c.ExecPodEnabled() {
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
