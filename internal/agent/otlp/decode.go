package otlp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ActiveTimeMetric is the only metric the receiver keeps.
const ActiveTimeMetric = "claude_code.active_time.total"

// Attribute keys the receiver keeps. Everything else is dropped in the
// decoder (see attrs).
const (
	attrSession    = "session.id"
	attrType       = "type"
	attrRepository = "vcs.repository.name"
)

// maxPoints bounds the datapoints read from one request.
const maxPoints = 10_000

// maxAttr bounds a kept attribute value, in bytes (the localdb limit).
const maxAttr = 256

// Point is one active-time datapoint as Claude Code sent it, reduced to the
// fields Logbook keeps.
type Point struct {
	SessionID string
	Type      string // "user" or "cli"
	// Repository is vcs.repository.name, or "" (Claude Code adds it only with
	// OTEL_METRICS_INCLUDE_REPOSITORY=true).
	Repository string
	// Start is the datapoint's start time (zero if not sent); Time its end.
	Start, Time time.Time
	Value       float64
	// Cumulative is true when Value is a running total since Start, false
	// when it is the increment over [Start, Time).
	Cumulative bool
}

// Decoded is the result of decoding one export request.
type Decoded struct {
	Points []Point
	// Rejected counts active-time datapoints that were unusable (no
	// session.id, unknown type, bad times or value, unknown temporality).
	Rejected int
}

// ErrBadRequest wraps every reason a body can't be decoded at all.
var ErrBadRequest = errors.New("bad OTLP request")

// Decode reads an OTLP/HTTP JSON ExportMetricsServiceRequest and returns its
// active-time datapoints. Attributes are taken from the datapoint, falling
// back to the resource; only the three kept keys are ever read. It is pure.
func Decode(body []byte) (Decoded, error) {
	var req struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes attrs `json:"attributes"`
			} `json:"resource"`
			ScopeMetrics []struct {
				Metrics []struct {
					Name string `json:"name"`
					Sum  *struct {
						DataPoints             []dataPoint `json:"dataPoints"`
						AggregationTemporality temporality `json:"aggregationTemporality"`
					} `json:"sum"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		return Decoded{}, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	var out Decoded
	seen := 0
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != ActiveTimeMetric || m.Sum == nil {
					continue
				}
				for _, dp := range m.Sum.DataPoints {
					if seen++; seen > maxPoints {
						return Decoded{}, fmt.Errorf("%w: more than %d datapoints", ErrBadRequest, maxPoints)
					}
					p, ok := dp.point(rm.Resource.Attributes, m.Sum.AggregationTemporality)
					if !ok {
						out.Rejected++
						continue
					}
					out.Points = append(out.Points, p)
				}
			}
		}
	}
	return out, nil
}

type dataPoint struct {
	Attributes        attrs    `json:"attributes"`
	StartTimeUnixNano unixNano `json:"startTimeUnixNano"`
	TimeUnixNano      unixNano `json:"timeUnixNano"`
	AsDouble          *number  `json:"asDouble"`
	AsInt             *number  `json:"asInt"`
}

// point validates a datapoint and builds a Point; ok is false if unusable.
func (dp dataPoint) point(resource attrs, t temporality) (Point, bool) {
	get := func(k string) string {
		if v, ok := dp.Attributes[k]; ok {
			return v
		}
		return resource[k]
	}
	p := Point{
		SessionID:  get(attrSession),
		Type:       get(attrType),
		Repository: get(attrRepository),
		Start:      dp.StartTimeUnixNano.time(),
		Time:       dp.TimeUnixNano.time(),
	}
	switch {
	case dp.AsDouble != nil:
		p.Value = float64(*dp.AsDouble)
	case dp.AsInt != nil:
		p.Value = float64(*dp.AsInt)
	default:
		return Point{}, false
	}
	switch t {
	case temporalityDelta:
	case temporalityCumulative:
		p.Cumulative = true
	default:
		return Point{}, false
	}
	if p.SessionID == "" || (p.Type != "user" && p.Type != "cli") || p.Time.IsZero() ||
		(!p.Start.IsZero() && p.Start.After(p.Time)) || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 {
		return Point{}, false
	}
	return p, true
}

// attrs decodes an OTLP KeyValue list, keeping only the string values of
// the keys Logbook uses. Other keys' values are skipped unread.
type attrs map[string]string

// UnmarshalJSON implements json.Unmarshaler.
func (a *attrs) UnmarshalJSON(b []byte) error {
	var kvs []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &kvs); err != nil {
		return fmt.Errorf("attributes: %w", err)
	}
	out := attrs{}
	for _, kv := range kvs {
		switch kv.Key {
		case attrSession, attrType, attrRepository:
		default:
			continue // identity and everything else: never read
		}
		var v struct {
			StringValue *string `json:"stringValue"`
		}
		if json.Unmarshal(kv.Value, &v) != nil || v.StringValue == nil {
			continue
		}
		if s := clean(*v.StringValue); s != "" {
			out[kv.Key] = s
		}
	}
	*a = out
	return nil
}

// clean drops a value with control characters or invalid UTF-8, and cuts
// nothing: a value over maxAttr is dropped too, since a cut id would be a
// different id.
func clean(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxAttr || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return ""
	}
	return s
}

// temporality is OTLP's AggregationTemporality, sent as an integer by the
// JSON encoding (and accepted by name, as protojson allows).
type temporality int

const (
	temporalityDelta      temporality = 1
	temporalityCumulative temporality = 2
)

// UnmarshalJSON implements json.Unmarshaler.
func (t *temporality) UnmarshalJSON(b []byte) error {
	var n int
	if json.Unmarshal(b, &n) == nil {
		*t = temporality(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("aggregationTemporality: %w", err)
	}
	switch s {
	case "AGGREGATION_TEMPORALITY_DELTA":
		*t = temporalityDelta
	case "AGGREGATION_TEMPORALITY_CUMULATIVE":
		*t = temporalityCumulative
	default:
		*t = 0
	}
	return nil
}

// unixNano is a fixed64 timestamp, sent as a decimal string by the OTLP
// JSON encoding (a plain number is accepted too). Zero means unset.
type unixNano uint64

// UnmarshalJSON implements json.Unmarshaler.
func (u *unixNano) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*u = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("timestamp %q: %w", s, err)
	}
	*u = unixNano(n)
	return nil
}

func (u unixNano) time() time.Time {
	if u == 0 || u > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(0, int64(u)).UTC()
}

// number is a datapoint value: a JSON number, or a string (asInt is an int64
// and so a decimal string in OTLP JSON). Non-finite values are rejected
// later by point.
type number float64

// UnmarshalJSON implements json.Unmarshaler.
func (n *number) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// protojson writes "NaN", "Infinity", "-Infinity" for doubles.
		*n = number(math.NaN())
		return nil //nolint:nilerr // an unusable value rejects the point, not the request
	}
	*n = number(f)
	return nil
}
