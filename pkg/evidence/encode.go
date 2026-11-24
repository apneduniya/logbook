package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Row type names used in the canonical encoding's "type" field. They are
// part of the format: renaming one changes every content hash.
const (
	TypeActivity   = "activity"
	TypeActiveTime = "active_time"
	TypeAI         = "ai"
	TypeGit        = "git"
	TypeHeartbeat  = "heartbeat"
	TypeRepo       = "repo"
)

// types lists every row type, for Counts and validation.
var types = []string{TypeActivity, TypeActiveTime, TypeAI, TypeGit, TypeHeartbeat, TypeRepo}

// ActiveTime is one Claude Code OpenTelemetry active-time datapoint as it
// appears in evidence: a delta over [From, To). It mirrors the stored row,
// without any field derived from other tables (such as the session's
// working directory), and is defined here, with JSON tags, so the evidence
// format doesn't change when the engine's types do.
type ActiveTime struct {
	From      time.Time    `json:"from"`
	To        time.Time    `json:"to"`
	SessionID string       `json:"session_id,omitempty"`
	UserSecs  float64      `json:"user_secs"`
	CLISecs   float64      `json:"cli_secs"`
	Harness   wire.Harness `json:"harness,omitempty"`
}

// Repo is one row of the device's repo mapping journal: from the row on,
// files and working directories under Path belong to Project.
type Repo struct {
	Path    string `json:"path"`
	Project string `json:"project"`
}

// Row is one logged row of a device's event log (ADR 0017): every raw event
// the struggle engine can read, and every repo mapping row, gets a sequence
// number from one counter when it is stored. Build rows with the
// constructors (ActivityRow and the others), which set Type and Time from
// the value.
type Row struct {
	// Seq is the row's sequence number: positive and unique in a block.
	Seq int64
	// Type is one of the Type* names and says what Value holds.
	Type string
	// Time is the row's own time, the "t" of its line. It decides the
	// row's minute bucket (BucketKey).
	Time time.Time
	// Value is the stored row: wire.ActivitySample, wire.Heartbeat,
	// wire.AIEvent, wire.GitEvent, ActiveTime or Repo.
	Value any
}

// ActivityRow is an activity sample's row; its time is the sample's.
func ActivityRow(seq int64, e wire.ActivitySample) Row {
	e.Time = e.Time.UTC()
	return Row{Seq: seq, Type: TypeActivity, Time: e.Time, Value: e}
}

// HeartbeatRow is a heartbeat's row, kept exactly as received; its time is
// the heartbeat's Timestamp().
func HeartbeatRow(seq int64, h wire.Heartbeat) Row {
	return Row{Seq: seq, Type: TypeHeartbeat, Time: h.Timestamp().UTC(), Value: h}
}

// AIRow is an AI event's row; its time is the event's.
func AIRow(seq int64, e wire.AIEvent) Row {
	e.Time = e.Time.UTC()
	return Row{Seq: seq, Type: TypeAI, Time: e.Time, Value: e}
}

// GitRow is a git event's row; its time is the event's.
func GitRow(seq int64, e wire.GitEvent) Row {
	e.Time = e.Time.UTC()
	return Row{Seq: seq, Type: TypeGit, Time: e.Time, Value: e}
}

// ActiveTimeRow is an active-time datapoint's row; its time is To.
func ActiveTimeRow(seq int64, a ActiveTime) Row {
	a.From, a.To = a.From.UTC(), a.To.UTC()
	return Row{Seq: seq, Type: TypeActiveTime, Time: a.To, Value: a}
}

// RepoRow is a repo mapping row; its time is when the device recorded it.
func RepoRow(seq int64, since time.Time, r Repo) Row {
	return Row{Seq: seq, Type: TypeRepo, Time: since.UTC(), Value: r}
}

var (
	// ErrInvalidRow is wrapped when a row can't be hashed: a sequence
	// number that isn't positive or appears twice, an unknown type, a
	// minute before 1970, or rows out of (bucket key, sequence) order.
	ErrInvalidRow = errors.New("invalid evidence row")
	// ErrInvalidLine is wrapped when a line isn't the canonical encoding of
	// a row.
	ErrInvalidLine = errors.New("invalid evidence line")
)

// lineJSON fixes the key order of an encoded line: s, t, type, v.
type lineJSON struct {
	S    int64           `json:"s"`
	T    string          `json:"t"`
	Type string          `json:"type"`
	V    json.RawMessage `json:"v"`
}

// Line returns the row's canonical line (ADR 0017 point 8), without the
// trailing "\n": the JSON object {"s": <sequence number>, "t": <RFC 3339
// UTC time with nanoseconds>, "type": <row type>, "v": <row>}.
//
// The format is fixed by:
//   - v is the row encoded by encoding/json without HTML escaping, so the
//     JSON field order is the Go struct field order of the wire types
//     (wire.ActivitySample, wire.Heartbeat, wire.AIEvent, wire.GitEvent) and
//     of ActiveTime and Repo. Reordering, renaming or retagging a field of
//     those structs changes the format, and the golden tests fail.
//   - every time.Time inside v is in UTC (the constructors convert it); a
//     heartbeat keeps its float64 Unix time exactly as received.
//   - t is the row's time (Row.Time), in UTC.
func (r Row) Line() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	v, err := marshal(r.Value)
	if err != nil {
		return nil, err
	}
	return marshal(lineJSON{S: r.Seq, T: r.Time.UTC().Format(time.RFC3339Nano), Type: r.Type, V: v})
}

// validate checks the parts of a row every format relies on.
func (r Row) validate() error {
	if r.Seq <= 0 {
		return fmt.Errorf("%w: sequence number %d", ErrInvalidRow, r.Seq)
	}
	if !slices.Contains(types, r.Type) {
		return fmt.Errorf("%w: type %q", ErrInvalidRow, r.Type)
	}
	return nil
}

// DecodeLine parses a canonical line (without its "\n") back into the row
// it encodes. The line must be exactly what Line returns for that row:
// unknown or reordered fields, a "t" that isn't the value's time, or any
// other difference is rejected with ErrInvalidLine. So the row a verifier
// reads is exactly the row that was hashed.
func DecodeLine(line []byte) (Row, error) {
	var l lineJSON
	if err := json.Unmarshal(line, &l); err != nil {
		return Row{}, fmt.Errorf("%w: %w", ErrInvalidLine, err)
	}
	r, err := decodeValue(l)
	if err != nil {
		return Row{}, fmt.Errorf("%w: %w", ErrInvalidLine, err)
	}
	again, err := r.Line()
	if err != nil {
		return Row{}, fmt.Errorf("%w: %w", ErrInvalidLine, err)
	}
	if !bytes.Equal(again, line) {
		return Row{}, fmt.Errorf("%w: not in canonical form (seq %d)", ErrInvalidLine, l.S)
	}
	return r, nil
}

// decodeValue builds the row a decoded line describes, with the
// constructor of its type.
func decodeValue(l lineJSON) (Row, error) {
	switch l.Type {
	case TypeActivity:
		return decodeAs(l.V, func(v wire.ActivitySample) Row { return ActivityRow(l.S, v) })
	case TypeHeartbeat:
		return decodeAs(l.V, func(v wire.Heartbeat) Row { return HeartbeatRow(l.S, v) })
	case TypeAI:
		return decodeAs(l.V, func(v wire.AIEvent) Row { return AIRow(l.S, v) })
	case TypeGit:
		return decodeAs(l.V, func(v wire.GitEvent) Row { return GitRow(l.S, v) })
	case TypeActiveTime:
		return decodeAs(l.V, func(v ActiveTime) Row { return ActiveTimeRow(l.S, v) })
	case TypeRepo:
		since, err := time.Parse(time.RFC3339Nano, l.T)
		if err != nil {
			return Row{}, fmt.Errorf("repo time: %w", err)
		}
		return decodeAs(l.V, func(v Repo) Row { return RepoRow(l.S, since, v) })
	default:
		return Row{}, fmt.Errorf("type %q", l.Type)
	}
}

// decodeAs decodes raw as a T and builds its row.
func decodeAs[T any](raw json.RawMessage, build func(T) Row) (Row, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return Row{}, err
	}
	return build(v), nil
}

// Counts returns how many rows of each type rows holds, keyed by the type
// names above. Every type is present, with 0 when there are none. It is the
// event_counts field of an uploaded evidence day.
func Counts(rows []Row) map[string]int {
	out := make(map[string]int, len(types))
	for _, t := range types {
		out[t] = 0
	}
	for _, r := range rows {
		out[r.Type]++
	}
	return out
}

// DayBounds returns the UTC start and end of day (YYYY-MM-DD).
func DayBounds(day string) (start, end time.Time, err error) {
	start, err = time.ParseInLocation(time.DateOnly, day, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: day %q", ErrInvalidDay, day)
	}
	return start, start.AddDate(0, 0, 1), nil
}

// marshal encodes v as compact JSON without HTML escaping and without the
// trailing newline json.Encoder adds.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding evidence row: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
