package evidence

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Event type names used in the canonical encoding's "type" field. They are
// part of the format: renaming one changes every content hash.
const (
	TypeActivity   = "activity"
	TypeActiveTime = "active_time"
	TypeAI         = "ai"
	TypeGit        = "git"
	TypeHeartbeat  = "heartbeat"
)

// ActiveTime is one Claude Code OpenTelemetry active-time datapoint as it
// appears in evidence: a delta over [From, To). It mirrors
// struggle.ActiveTime but is defined here, with JSON tags, so the evidence
// format doesn't change when the engine's types do.
type ActiveTime struct {
	From       time.Time    `json:"from"`
	To         time.Time    `json:"to"`
	SessionID  string       `json:"session_id,omitempty"`
	UserSecs   float64      `json:"user_secs"`
	CLISecs    float64      `json:"cli_secs"`
	Harness    wire.Harness `json:"harness,omitempty"`
	ProjectCwd string       `json:"project_cwd,omitempty"`
}

// DayEvents is one UTC day's raw events from one device: everything the
// day's content hash covers. Slices need not be sorted.
type DayEvents struct {
	// Day is the UTC date, YYYY-MM-DD.
	Day        string
	Activity   []wire.ActivitySample
	Heartbeats []wire.Heartbeat
	AI         []wire.AIEvent
	Git        []wire.GitEvent
	// ActiveTime datapoints belong to the day their To falls in.
	ActiveTime []ActiveTime
}

// ErrOutsideDay is wrapped when an event's time is not within DayEvents.Day.
var ErrOutsideDay = errors.New("event outside the evidence day")

// line is one event of the canonical encoding.
type line struct {
	t   time.Time
	typ string
	v   []byte // canonical JSON of the event
}

// lineJSON fixes the key order of an encoded line: t, type, v.
type lineJSON struct {
	T    string          `json:"t"`
	Type string          `json:"type"`
	V    json.RawMessage `json:"v"`
}

// Encode writes the canonical encoding of d to w: JSON Lines, one object
// {"t": <RFC 3339 UTC time with nanoseconds>, "type": <event type>, "v":
// <event>} per event, each line ending in "\n", sorted by time, then type,
// then the bytes of v.
//
// The format is fixed by:
//   - v is the event encoded by encoding/json without HTML escaping, so the
//     JSON field order is the Go struct field order of the wire types
//     (wire.ActivitySample, wire.Heartbeat, wire.AIEvent, wire.GitEvent) and
//     of ActiveTime. Reordering, renaming or retagging a field of those
//     structs changes the format, and TestContentHashGolden fails.
//   - every time.Time inside v is converted to UTC first; a heartbeat keeps
//     its float64 Unix time exactly as received.
//   - t is the event's time: Time for activity, AI and git events,
//     Timestamp() for heartbeats and To for active-time datapoints.
//
// Events whose t falls outside [Day 00:00 UTC, next day 00:00 UTC) are
// rejected with ErrOutsideDay rather than silently dropped.
func Encode(w io.Writer, d DayEvents) error {
	lines, err := canonicalLines(d)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	for _, l := range lines {
		b, err := marshal(lineJSON{T: l.t.Format(time.RFC3339Nano), Type: l.typ, V: l.v})
		if err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return fmt.Errorf("writing evidence: %w", err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("writing evidence: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("writing evidence: %w", err)
	}
	return nil
}

// ContentHash returns the hex SHA-256 of Encode(d): the content_hash of an
// evidence Day.
func ContentHash(d DayEvents) (string, error) {
	h := sha256.New()
	if err := Encode(h, d); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Counts returns how many events of each type d holds, keyed by the type
// names above. Every type is present, with 0 when there are none. It is the
// event_counts field of an uploaded evidence day.
func Counts(d DayEvents) map[string]int {
	return map[string]int{
		TypeActivity:   len(d.Activity),
		TypeActiveTime: len(d.ActiveTime),
		TypeAI:         len(d.AI),
		TypeGit:        len(d.Git),
		TypeHeartbeat:  len(d.Heartbeats),
	}
}

// DayBounds returns the UTC start and end of day (YYYY-MM-DD).
func DayBounds(day string) (start, end time.Time, err error) {
	start, err = time.ParseInLocation(time.DateOnly, day, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: day %q", ErrInvalidDay, day)
	}
	return start, start.AddDate(0, 0, 1), nil
}

// canonicalLines encodes every event of d and sorts them.
func canonicalLines(d DayEvents) ([]line, error) {
	start, end, err := DayBounds(d.Day)
	if err != nil {
		return nil, err
	}
	n := len(d.Activity) + len(d.Heartbeats) + len(d.AI) + len(d.Git) + len(d.ActiveTime)
	lines := make([]line, 0, n)
	add := func(t time.Time, typ string, v any) error {
		t = t.UTC()
		if t.Before(start) || !t.Before(end) {
			return fmt.Errorf("%w: %s event at %s is not on %s", ErrOutsideDay, typ, t.Format(time.RFC3339Nano), d.Day)
		}
		b, err := marshal(v)
		if err != nil {
			return err
		}
		lines = append(lines, line{t: t, typ: typ, v: b})
		return nil
	}
	for _, e := range d.Activity {
		e.Time = e.Time.UTC()
		if err := add(e.Time, TypeActivity, e); err != nil {
			return nil, err
		}
	}
	for _, e := range d.Heartbeats {
		if err := add(e.Timestamp(), TypeHeartbeat, e); err != nil {
			return nil, err
		}
	}
	for _, e := range d.AI {
		e.Time = e.Time.UTC()
		if err := add(e.Time, TypeAI, e); err != nil {
			return nil, err
		}
	}
	for _, e := range d.Git {
		e.Time = e.Time.UTC()
		if err := add(e.Time, TypeGit, e); err != nil {
			return nil, err
		}
	}
	for _, e := range d.ActiveTime {
		e.From, e.To = e.From.UTC(), e.To.UTC()
		if err := add(e.To, TypeActiveTime, e); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(lines, func(a, b line) int {
		if c := a.t.Compare(b.t); c != 0 {
			return c
		}
		if a.typ != b.typ {
			if a.typ < b.typ {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.v, b.v)
	})
	return lines, nil
}

// marshal encodes v as compact JSON without HTML escaping and without the
// trailing newline json.Encoder adds.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding evidence event: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
