// Package wire defines the data formats exchanged between editor plugins,
// the Logbook agent and the Logbook server. It owns encoding and validation
// of those formats only; it does no I/O and holds no business rules.
package wire

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// MaxBulkHeartbeats is the largest batch accepted by the bulk endpoint.
// It matches Hackatime's limit so existing clients behave the same.
const MaxBulkHeartbeats = 100

// Limits that keep a single heartbeat bounded in size.
const (
	maxEntityLen = 1024
	maxShortLen  = 256
	maxDeps      = 100
)

// earliestTime rejects obviously wrong clocks; nothing older than WakaTime itself.
var earliestTime = time.Date(2013, 1, 1, 0, 0, 0, 0, time.UTC)

// Heartbeat is one WakaTime-format heartbeat, including Hackatime's AI fields.
// Field names and JSON tags match the WakaTime API exactly (ADR 0005).
type Heartbeat struct {
	Entity           string   `json:"entity"`
	Type             string   `json:"type,omitempty"`
	Category         string   `json:"category,omitempty"`
	Time             float64  `json:"time"`
	Project          string   `json:"project,omitempty"`
	ProjectRootCount *int     `json:"project_root_count,omitempty"`
	Branch           string   `json:"branch,omitempty"`
	Language         string   `json:"language,omitempty"`
	Dependencies     []string `json:"dependencies,omitempty"`
	Lines            *int     `json:"lines,omitempty"`
	LineAdditions    *int     `json:"line_additions,omitempty"`
	LineDeletions    *int     `json:"line_deletions,omitempty"`
	Lineno           *int     `json:"lineno,omitempty"`
	Cursorpos        *int     `json:"cursorpos,omitempty"`
	IsWrite          bool     `json:"is_write,omitempty"`
	Editor           string   `json:"editor,omitempty"`
	Plugin           string   `json:"plugin,omitempty"`
	Machine          string   `json:"machine,omitempty"`
	OperatingSystem  string   `json:"operating_system,omitempty"`
	UserAgent        string   `json:"user_agent,omitempty"`

	AIModel          string `json:"ai_model,omitempty"`
	AISession        string `json:"ai_session,omitempty"`
	AIInputTokens    *int64 `json:"ai_input_tokens,omitempty"`
	AIOutputTokens   *int64 `json:"ai_output_tokens,omitempty"`
	AIPromptLength   *int   `json:"ai_prompt_length,omitempty"`
	AILineChanges    *int   `json:"ai_line_changes,omitempty"`
	HumanLineChanges *int   `json:"human_line_changes,omitempty"`
}

// ErrInvalidHeartbeat is wrapped by every validation failure.
var ErrInvalidHeartbeat = errors.New("invalid heartbeat")

// Timestamp returns the heartbeat time as UTC with microsecond precision.
func (h Heartbeat) Timestamp() time.Time {
	sec, frac := math.Modf(h.Time)
	return time.Unix(int64(sec), int64(frac*1e6)*1e3).UTC()
}

// Validate checks the heartbeat against the given clock. now is passed in so
// callers and tests control time.
func (h Heartbeat) Validate(now time.Time) error {
	switch {
	case strings.TrimSpace(h.Entity) == "":
		return fmt.Errorf("%w: entity is required", ErrInvalidHeartbeat)
	case len(h.Entity) > maxEntityLen:
		return fmt.Errorf("%w: entity longer than %d bytes", ErrInvalidHeartbeat, maxEntityLen)
	case math.IsNaN(h.Time) || math.IsInf(h.Time, 0):
		return fmt.Errorf("%w: time is not a number", ErrInvalidHeartbeat)
	case len(h.Dependencies) > maxDeps:
		return fmt.Errorf("%w: more than %d dependencies", ErrInvalidHeartbeat, maxDeps)
	}
	ts := h.Timestamp()
	if ts.Before(earliestTime) || ts.After(now.Add(time.Hour)) {
		return fmt.Errorf("%w: time %v out of range", ErrInvalidHeartbeat, ts)
	}
	for name, v := range map[string]string{
		"type": h.Type, "category": h.Category, "project": h.Project, "branch": h.Branch,
		"language": h.Language, "editor": h.Editor, "plugin": h.Plugin, "machine": h.Machine,
		"operating_system": h.OperatingSystem, "user_agent": h.UserAgent,
		"ai_model": h.AIModel, "ai_session": h.AISession,
	} {
		if len(v) > maxShortLen {
			return fmt.Errorf("%w: %s longer than %d bytes", ErrInvalidHeartbeat, name, maxShortLen)
		}
	}
	return nil
}

// FieldsHash identifies a heartbeat for deduplication within one user's data.
// Resending the same heartbeat yields the same hash; any field change yields a
// new one. The encoding is length-prefixed so field boundaries can't collide.
func (h Heartbeat) FieldsHash() []byte {
	d := sha256.New()
	writeStr := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		d.Write(n[:])
		d.Write([]byte(s))
	}
	writeInt := func(p *int) {
		if p == nil {
			writeStr("")
			return
		}
		writeStr(fmt.Sprint(*p))
	}
	writeStr(fmt.Sprintf("%.6f", h.Time))
	for _, s := range []string{h.Entity, h.Type, h.Category, h.Project, h.Branch, h.Language,
		h.Editor, h.Plugin, h.Machine, h.OperatingSystem, h.UserAgent, h.AIModel, h.AISession} {
		writeStr(s)
	}
	for _, p := range []*int{h.Lines, h.LineAdditions, h.LineDeletions, h.Lineno, h.Cursorpos, h.ProjectRootCount} {
		writeInt(p)
	}
	if h.IsWrite {
		writeStr("w")
	} else {
		writeStr("")
	}
	writeStr(strings.Join(h.Dependencies, "\x00"))
	return d.Sum(nil)
}
