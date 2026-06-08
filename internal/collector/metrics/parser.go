package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// ParsedFamily is a normalized Prometheus metric family ready for export.
type ParsedFamily struct {
	Name    string
	Help    string
	Type    agentv1.MetricType
	Metrics []ParsedMetric
}

// ParsedMetric is a single time series within a metric family.
type ParsedMetric struct {
	Labels    map[string]string
	Value     float64
	Timestamp int64
}

// MetricFilter applies allow/deny regex patterns to metric family names.
type MetricFilter struct {
	allow []*regexp.Regexp
	deny  []*regexp.Regexp
}

// NewMetricFilter compiles allow/deny regex patterns once at construction time.
func NewMetricFilter(allowPatterns, denyPatterns []string) (*MetricFilter, error) {
	allow, err := compilePatterns(allowPatterns)
	if err != nil {
		return nil, fmt.Errorf("compile metric allowlist: %w", err)
	}
	deny, err := compilePatterns(denyPatterns)
	if err != nil {
		return nil, fmt.Errorf("compile metric denylist: %w", err)
	}
	return &MetricFilter{allow: allow, deny: deny}, nil
}

// Allows reports whether a metric family name passes the configured filters.
func (f *MetricFilter) Allows(name string) bool {
	if f == nil {
		return true
	}
	for _, re := range f.deny {
		if re.MatchString(name) {
			return false
		}
	}
	if len(f.allow) == 0 {
		return true
	}
	for _, re := range f.allow {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// ParsePrometheusText parses Prometheus exposition format (text 0.0.4) from data.
func ParsePrometheusText(data []byte) ([]ParsedFamily, error) {
	dec := expfmt.NewDecoder(bytes.NewReader(data), expfmt.FmtText)
	var families []ParsedFamily
	for {
		var fam dto.MetricFamily
		if err := dec.Decode(&fam); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode prometheus metric family: %w", err)
		}
		parsed, err := familyFromDTO(&fam)
		if err != nil {
			return nil, err
		}
		if len(parsed.Metrics) > 0 {
			families = append(families, parsed)
		}
	}
	return families, nil
}

// FilterFamilies returns families whose names pass the filter.
func FilterFamilies(families []ParsedFamily, filter *MetricFilter) []ParsedFamily {
	if filter == nil {
		return families
	}
	out := make([]ParsedFamily, 0, len(families))
	for _, fam := range families {
		if filter.Allows(fam.Name) {
			out = append(out, fam)
		}
	}
	return out
}

// MergeLabels returns a copy of base with extra labels applied (extra wins on conflict).
func MergeLabels(base, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// FamilyJSON marshals a parsed family to JSON for queue export.
func FamilyJSON(fam ParsedFamily) (string, error) {
	payload := familyJSONPayload{
		Name:    fam.Name,
		Help:    fam.Help,
		Type:    fam.Type.String(),
		Metrics: fam.Metrics,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal metric family json: %w", err)
	}
	return string(b), nil
}

type familyJSONPayload struct {
	Name    string         `json:"name"`
	Help    string         `json:"help"`
	Type    string         `json:"type"`
	Metrics []ParsedMetric `json:"metrics"`
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func familyFromDTO(fam *dto.MetricFamily) (ParsedFamily, error) {
	if fam == nil {
		return ParsedFamily{}, fmt.Errorf("metric family is nil")
	}
	name := fam.GetName()
	out := ParsedFamily{
		Name: name,
		Help: fam.GetHelp(),
		Type: protoMetricType(fam.GetType()),
	}
	for _, m := range fam.GetMetric() {
		pm, ok := metricFromDTO(m)
		if !ok {
			continue
		}
		out.Metrics = append(out.Metrics, pm)
	}
	return out, nil
}

func metricFromDTO(m *dto.Metric) (ParsedMetric, bool) {
	if m == nil {
		return ParsedMetric{}, false
	}
	value, ok := sampleValue(m)
	if !ok {
		return ParsedMetric{}, false
	}
	labels := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	var ts int64
	if m.TimestampMs != nil {
		ts = m.GetTimestampMs()
	}
	return ParsedMetric{
		Labels:    labels,
		Value:     value,
		Timestamp: ts,
	}, true
}

func sampleValue(m *dto.Metric) (float64, bool) {
	switch {
	case m.Gauge != nil:
		return m.GetGauge().GetValue(), true
	case m.Counter != nil:
		return m.GetCounter().GetValue(), true
	case m.Untyped != nil:
		return m.GetUntyped().GetValue(), true
	case m.Summary != nil:
		return m.GetSummary().GetSampleSum(), true
	case m.Histogram != nil:
		return m.GetHistogram().GetSampleSum(), true
	default:
		return 0, false
	}
}

func protoMetricType(t dto.MetricType) agentv1.MetricType {
	switch t {
	case dto.MetricType_GAUGE:
		return agentv1.MetricType_METRIC_TYPE_GAUGE
	case dto.MetricType_COUNTER:
		return agentv1.MetricType_METRIC_TYPE_COUNTER
	case dto.MetricType_HISTOGRAM:
		return agentv1.MetricType_METRIC_TYPE_HISTOGRAM
	case dto.MetricType_SUMMARY:
		return agentv1.MetricType_METRIC_TYPE_SUMMARY
	default:
		return agentv1.MetricType_METRIC_TYPE_UNSPECIFIED
	}
}

// TimestampOrNow returns ts when positive, otherwise now in UTC.
func TimestampOrNow(ts int64) time.Time {
	if ts > 0 {
		return time.UnixMilli(ts).UTC()
	}
	return time.Now().UTC()
}
