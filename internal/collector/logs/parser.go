package logs

import (
	"encoding/json"
	"strings"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// highCardinalityLabelKeys are stripped from exported pod labels.
var highCardinalityLabelKeys = map[string]struct{}{
	"pod-template-hash":          {},
	"controller-revision-hash":   {},
	"batch.kubernetes.io/job-name": {},
}

// ParsedLine is the structured result of parsing a single log line.
type ParsedLine struct {
	Message   string
	Level     agentv1.LogLevel
	Raw       []byte
	Timestamp time.Time
	// Stream is the CRI marker: "stdout", "stderr", or "" when the line
	// carried no prefix (a log source that is not the kubelet's file format).
	Stream string
}

// ParseLine parses a log line, extracting level, message, and timestamp when
// present. The payload it carries (Raw, and Message when nothing overrides
// it) is the application's line as written: leading and internal whitespace
// are preserved verbatim. Only "\r"/"\n" left over from the transport's own
// line framing are stripped — that framing is not payload.
func ParseLine(line string, fallback time.Time) ParsedLine {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return ParsedLine{Timestamp: fallback}
	}

	msg := line
	ts := fallback
	stream := ""
	level := agentv1.LogLevel_LOG_LEVEL_UNSPECIFIED

	// Kubernetes log API prefix: "2006-01-02T15:04:05.999999999Z stdout F payload"
	if parsedTS, parsedStream, rest, ok := splitK8sLogPrefix(line); ok {
		ts = parsedTS
		stream = parsedStream
		msg = rest
	}

	// Captured AFTER the prefix split on purpose: raw is what reaches Loki as
	// the log line, and a line that still carries "2026-…Z stdout F " in front
	// of its JSON cannot be parsed by `| json` at query time.
	raw := []byte(msg)

	if pl, ok := parseJSONLine(msg); ok {
		if !pl.Timestamp.IsZero() {
			ts = pl.Timestamp
		}
		if pl.Message != "" {
			msg = pl.Message
		}
		if pl.Level != agentv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
			level = pl.Level
		}
	}

	return ParsedLine{
		Message:   msg,
		Level:     level,
		Raw:       raw,
		Timestamp: ts,
		Stream:    stream,
	}
}

func splitK8sLogPrefix(line string) (time.Time, string, string, bool) {
	// RFC3339Nano timestamp at line start (39+ chars).
	if len(line) < 30 || line[0] < '0' || line[0] > '9' {
		return time.Time{}, "", line, false
	}
	space := strings.IndexByte(line, ' ')
	if space <= 0 {
		return time.Time{}, "", line, false
	}
	tsPart := line[:space]
	parsed, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		if parsed, err = time.Parse(time.RFC3339, tsPart); err != nil {
			return time.Time{}, "", line, false
		}
	}
	rest := line[space+1:]
	stream := ""
	// Drop the "stdout F"/"stderr F" tripwire fields when present, keeping the
	// stream marker — it is a two-valued, indexable fact about the line. The
	// CRI log format separates timestamp/stream/tag/payload with exactly one
	// space each, so this only consumes those two known tokens and their
	// separators; anything past the tag — including whitespace the
	// application itself wrote — is left untouched.
	if payload, parsedStream, ok := stripStreamAndTag(rest); ok {
		stream = parsedStream
		rest = payload
	}
	return parsed.UTC(), stream, rest, true
}

// stripStreamAndTag splits "stdout F payload" (or "stderr P payload") into
// its stream marker and payload. It does not touch whitespace inside the
// payload — the app may have indented it, and that indentation is part of
// the verbatim line.
func stripStreamAndTag(s string) (payload, stream string, ok bool) {
	streamEnd := strings.IndexByte(s, ' ')
	if streamEnd <= 0 {
		return s, "", false
	}
	streamPart := s[:streamEnd]
	if streamPart != "stdout" && streamPart != "stderr" {
		return s, "", false
	}
	afterStream := s[streamEnd+1:]
	tagEnd := strings.IndexByte(afterStream, ' ')
	if tagEnd != 1 {
		// The tag is always exactly one character ("F" full / "P" partial).
		return s, "", false
	}
	return afterStream[tagEnd+1:], streamPart, true
}

func parseJSONLine(line string) (ParsedLine, bool) {
	// The application may have indented its own JSON output; that leading
	// whitespace is payload and stays in Raw. It is only stripped here, for
	// detection, and json.Unmarshal itself skips insignificant leading
	// whitespace when parsing the value below.
	if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "{") {
		return ParsedLine{}, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return ParsedLine{}, false
	}

	pl := ParsedLine{
		Level: mapLogLevel(extractString(obj, "level")),
	}
	pl.Message = firstNonEmpty(
		extractString(obj, "msg"),
		extractString(obj, "message"),
	)
	pl.Timestamp = extractTime(obj)
	return pl, true
}

func extractString(obj map[string]any, key string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func extractTime(obj map[string]any) time.Time {
	for _, key := range []string{"time", "timestamp", "ts"} {
		v, ok := obj[key]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return parsed.UTC()
			}
			if parsed, err := time.Parse(time.RFC3339, t); err == nil {
				return parsed.UTC()
			}
		case float64:
			return unixTimeFromNumber(t)
		}
	}
	return time.Time{}
}

func unixTimeFromNumber(v float64) time.Time {
	if v > 1e12 {
		return time.UnixMilli(int64(v)).UTC()
	}
	return time.Unix(int64(v), 0).UTC()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// mapLogLevel maps common level strings to proto LogLevel values.
func mapLogLevel(level string) agentv1.LogLevel {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "trace":
		return agentv1.LogLevel_LOG_LEVEL_DEBUG
	case "info":
		return agentv1.LogLevel_LOG_LEVEL_INFO
	case "warn", "warning":
		return agentv1.LogLevel_LOG_LEVEL_WARN
	case "error", "err":
		return agentv1.LogLevel_LOG_LEVEL_ERROR
	case "fatal", "panic", "critical":
		return agentv1.LogLevel_LOG_LEVEL_ERROR
	default:
		return agentv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
}

// FilterPodLabels removes high-cardinality Kubernetes labels from exports.
func FilterPodLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if _, skip := highCardinalityLabelKeys[k]; skip {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LevelLabel returns a stable metric label for a log level.
func LevelLabel(level agentv1.LogLevel) string {
	switch level {
	case agentv1.LogLevel_LOG_LEVEL_DEBUG:
		return "debug"
	case agentv1.LogLevel_LOG_LEVEL_INFO:
		return "info"
	case agentv1.LogLevel_LOG_LEVEL_WARN:
		return "warn"
	case agentv1.LogLevel_LOG_LEVEL_ERROR:
		return "error"
	default:
		return "unknown"
	}
}
