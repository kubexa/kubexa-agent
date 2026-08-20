package config

import (
	"bytes"
	"errors"
	"regexp"

	"gopkg.in/yaml.v3"
)

// unknownFieldError matches what yaml.v3 puts in a KnownFields error, as in
// `line 12: field podNames not found in type config.LogNamespaceRule`. The
// line, the key and the rejecting type are all in that one string, which is
// why the warnings are passed through verbatim rather than reformatted: the
// type name is what tells an operator which block the key came from.
//
// It is anchored on the whole message rather than matched as a substring so a
// type error quoting a value that happens to contain the phrase cannot be
// reported as an unknown key.
var unknownFieldError = regexp.MustCompile(`^(line \d+: )?field \S+ not found in type \S+$`)

// UnknownKeys returns one warning per key in data that no config field
// accepts. It never fails: a key the agent does not know is dropped in
// silence today, and this only makes the drop audible.
//
// It matters most for the `rules` blocks. The Helm chart renders those with
// toYaml, so their keys are the agent's own snake_case config keys while
// every chart value around them is camelCase. Writing `podNames` there
// produces a rule with no pod filter at all -- which collects everything
// rather than nothing, so it reads as an over-broad rule and never as a typo.
//
// Type errors are deliberately not reported. Load's own parse pass already
// refuses to start on those, and repeating them here would attach "unknown
// key" to a key that is known.
//
// Only the first YAML document is examined, matching what Load itself binds:
// yaml.Unmarshal also reads one document and drops any that follow. The chart
// never renders more than one.
func UnknownKeys(data []byte) []string {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var probe Config
	err := decoder.Decode(&probe)
	if err == nil {
		return nil
	}

	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		// Unparseable YAML, or an empty document. Load reports the first.
		return nil
	}

	warnings := make([]string, 0, len(typeErr.Errors))
	for _, e := range typeErr.Errors {
		if unknownFieldError.MatchString(e) {
			warnings = append(warnings, e)
		}
	}
	if len(warnings) == 0 {
		return nil
	}
	return warnings
}
