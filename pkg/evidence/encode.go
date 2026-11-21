package evidence

import (
	"bufio"
	"bytes"
	"cmp"
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
	// Time is the row's own time, the "t" of its line.
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

// ErrInvalidRow is wrapped when a row can't be encoded: a sequence number
// that isn't positive or appears twice, or an unknown type.
var ErrInvalidRow = errors.New("invalid evidence row")

// lineJSON fixes the key order of an encoded line: s, t, type, v.
type lineJSON struct {
	S    int64           `json:"s"`
	T    string          `json:"t"`
	Type string          `json:"type"`
	V    json.RawMessage `json:"v"`
}

// Encode writes the canonical encoding of a block of logged rows to w:
// JSON Lines, one object {"s": <sequence number>, "t": <RFC 3339 UTC time
// with nanoseconds>, "type": <row type>, "v": <row>} per row, each line
// ending in "\n", sorted by s. Sequence numbers are unique, so the order is
// total and doesn't depend on the order of rows.
//
// The format is fixed by:
//   - v is the row encoded by encoding/json without HTML escaping, so the
//     JSON field order is the Go struct field order of the wire types
//     (wire.ActivitySample, wire.Heartbeat, wire.AIEvent, wire.GitEvent) and
//     of ActiveTime and Repo. Reordering, renaming or retagging a field of
//     those structs changes the format, and TestContentHashGolden fails.
//   - every time.Time inside v is in UTC (the constructors convert it); a
//     heartbeat keeps its float64 Unix time exactly as received.
//   - t is the row's time (Row.Time), in UTC.
//
// A row with a sequence number that isn't positive or repeats, or with an
// unknown type, is rejected with ErrInvalidRow.
func Encode(w io.Writer, rows []Row) error {
	sorted, err := sortedRows(rows)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	for _, r := range sorted {
		v, err := marshal(r.Value)
		if err != nil {
			return err
		}
		b, err := marshal(lineJSON{S: r.Seq, T: r.Time.UTC().Format(time.RFC3339Nano), Type: r.Type, V: v})
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

// ContentHash returns the hex SHA-256 of Encode(rows): the content_hash of
// an evidence Day.
func ContentHash(rows []Row) (string, error) {
	h := sha256.New()
	if err := Encode(h, rows); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// sortedRows validates rows and returns them sorted by sequence number.
func sortedRows(rows []Row) ([]Row, error) {
	out := slices.Clone(rows)
	slices.SortFunc(out, func(a, b Row) int { return cmp.Compare(a.Seq, b.Seq) })
	for i, r := range out {
		if r.Seq <= 0 {
			return nil, fmt.Errorf("%w: sequence number %d", ErrInvalidRow, r.Seq)
		}
		if i > 0 && out[i-1].Seq == r.Seq {
			return nil, fmt.Errorf("%w: sequence number %d appears twice", ErrInvalidRow, r.Seq)
		}
		if !slices.Contains(types, r.Type) {
			return nil, fmt.Errorf("%w: type %q", ErrInvalidRow, r.Type)
		}
	}
	return out, nil
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
