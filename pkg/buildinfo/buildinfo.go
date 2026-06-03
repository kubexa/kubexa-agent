// Package buildinfo exposes agent release metadata injected at link time.
package buildinfo

// Version is the agent release version (set via -ldflags).
var Version = "dev"

// Commit is the git commit SHA at build time (set via -ldflags).
var Commit = "unknown"

// BuildTime is the UTC build timestamp (set via -ldflags).
var BuildTime = "unknown"

// LogFields returns version metadata as structured log fields.
func LogFields() map[string]string {
	return map[string]string{
		"version":    Version,
		"commit":     Commit,
		"build_time": BuildTime,
	}
}
