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
}

// ParseLine trims and parses a log line, extracting level, message, and timestamp when present.
func ParseLine(line string, fallback time.Time) ParsedLine {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ParsedLine{Timestamp: fallback}
	}

	raw := []byte(trimmed)
	msg := trimmed
	ts := fallback
	level := agentv1.LogLevel_LOG_LEVEL_UNSPECIFIED

	// Kubernetes log API prefix: "2006-01-02T15:04:05.999999999Z stdout F payload"
	if parsedTS, rest, ok := splitK8sLogPrefix(trimmed); ok {
		ts = parsedTS
		msg = rest
	}

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
		return ParsedLine{
			Message:   msg,
			Level:     level,
			Raw:       raw,
			Timestamp: ts,
		}
	}

	return ParsedLine{
		Message:   msg,
		Level:     level,
		Raw:       raw,
		Timestamp: ts,
	}
}

func splitK8sLogPrefix(line string) (time.Time, string, bool) {
	// RFC3339Nano timestamp at line start (39+ chars).
	if len(line) < 30 || line[0] < '0' || line[0] > '9' {
		return time.Time{}, line, false
	}
	space := strings.IndexByte(line, ' ')
	if space <= 0 {
		return time.Time{}, line, false
	}
	tsPart := line[:space]
	parsed, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		if parsed, err = time.Parse(time.RFC3339, tsPart); err != nil {
			return time.Time{}, line, false
		}
	}
	rest := strings.TrimSpace(line[space+1:])
	// Drop "stdout F" or "stderr F" tripwire fields when present.
	if parts := strings.Fields(rest); len(parts) >= 3 && (parts[0] == "stdout" || parts[0] == "stderr") && len(parts[1]) == 1 {
		rest = strings.Join(parts[2:], " ")
	}
	return parsed.UTC(), rest, true
}

func parseJSONLine(line string) (ParsedLine, bool) {
	if !strings.HasPrefix(line, "{") {
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
