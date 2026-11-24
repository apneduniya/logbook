package replay

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Window arithmetic (ADR 0010, ADR 0017).
const (
	// Window is how many minutes before T a run computes.
	Window = 60 * time.Minute
	// History is the extra context the engine reads before the window: its
	// whole look-back, so every minute in the window is computed from all
	// the events its rules read.
	History = struggle.ContextBefore
)

// Key is a computation key (ADR 0017): T is the end of the run's window, a
// whole minute; H is the log's high-water mark in the run's snapshot.
type Key struct {
	T time.Time
	H int64
}

// ErrInvalidKey is wrapped when a key can't be replayed.
var ErrInvalidKey = errors.New("invalid computation key")

// Validate checks that T is a whole minute and H isn't negative.
func (k Key) Validate() error {
	if k.T.IsZero() || !k.T.Equal(k.T.Truncate(time.Minute)) {
		return fmt.Errorf("%w: T %s is not a whole minute", ErrInvalidKey, k.T.Format(time.RFC3339Nano))
	}
	if k.H < 0 {
		return fmt.Errorf("%w: H %d", ErrInvalidKey, k.H)
	}
	return nil
}

// Span is the time ranges of the run whose window ends at To, all UTC.
type Span struct {
	// History is where the events the run reads start: From - History.
	History time.Time
	// From and To bound the minutes the run computes, [From, To).
	From, To time.Time
}

// SpanAt returns the span of the run whose window ends at the whole
// minute t is in.
func SpanAt(t time.Time) Span {
	to := t.UTC().Truncate(time.Minute)
	from := to.Add(-Window)
	return Span{History: from.Add(-History), From: from, To: to}
}

// Span returns the span of the run at k.
func (k Key) Span() Span { return SpanAt(k.T) }

// Input builds the engine's input for key from logged rows, in any order
// (ADR 0017, ADR 0019):
//   - only rows with a sequence number at or below key.H count;
//   - activity samples, heartbeats and AI events with a time in [T - 120
//     min, T) are the events, the window is [T - 60 min, T) and the data
//     ends at T;
//   - the repo mapping is the repo rows reduced to the highest sequence
//     number per exact path (Repos);
//   - git events are never engine input, and active-time rows are left
//     out: they are intervals read outside the run's snapshot and feed only
//     the otel_mismatch flag, which replays don't compare (ADR 0019 point
//     8). Input never sets ActiveTime.
//
// tz is the user's time zone (UTC when nil); it too feeds only flags.
func Input(rows []evidence.Row, key Key, tz *time.Location) (struggle.Input, error) {
	return InputWithRepos(rows, Repos(rows, key.H), key, tz)
}

// InputWithRepos is Input with the repo mapping at key.H already resolved,
// as the agent reads it from its log; repo rows in rows are ignored. The
// agent's live runs build their input with it, then add Claude Code active
// time (which replays leave out). Every other part is the same code a
// replay runs.
func InputWithRepos(rows []evidence.Row, repos []struggle.Repo, key Key, tz *time.Location) (struggle.Input, error) {
	if err := key.Validate(); err != nil {
		return struggle.Input{}, err
	}
	if tz == nil {
		tz = time.UTC
	}
	s := key.Span()
	in := struggle.Input{
		Window:  struggle.Window{From: s.From, To: s.To},
		DataEnd: s.To,
		Repos:   repos,
		TZ:      tz,
	}
	for _, r := range picked(rows, key.H, s) {
		var ok bool
		switch r.Type {
		case evidence.TypeActivity:
			var v wire.ActivitySample
			if v, ok = r.Value.(wire.ActivitySample); ok {
				in.Activity = append(in.Activity, v)
			}
		case evidence.TypeHeartbeat:
			var v wire.Heartbeat
			if v, ok = r.Value.(wire.Heartbeat); ok {
				in.Heartbeats = append(in.Heartbeats, engineHeartbeat(v))
			}
		case evidence.TypeAI:
			var v wire.AIEvent
			if v, ok = r.Value.(wire.AIEvent); ok {
				in.AI = append(in.AI, v)
			}
		}
		if !ok {
			return struggle.Input{}, fmt.Errorf("replay: seq %d: a %s row holds %T", r.Seq, r.Type, r.Value)
		}
	}
	return in, nil
}

// picked returns the engine's event rows of rows for a run at high-water
// mark h over span s, in the order the agent's log reads them: by stored
// time (heartbeats are stored to the millisecond), then sequence number.
// The engine sorts its input, but not stably, so equal times must come in
// the same order on every path.
func picked(rows []evidence.Row, h int64, s Span) []evidence.Row {
	var out []evidence.Row
	for _, r := range rows {
		switch r.Type {
		case evidence.TypeActivity, evidence.TypeHeartbeat, evidence.TypeAI:
		default:
			continue
		}
		if r.Seq <= 0 || r.Seq > h || r.Time.Before(s.History) || !r.Time.Before(s.To) {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b evidence.Row) int {
		return cmp.Or(cmp.Compare(storedTime(a), storedTime(b)), cmp.Compare(a.Seq, b.Seq))
	})
	return out
}

// storedTime is a row's time as the agent's log stores and orders it: Unix
// milliseconds.
func storedTime(r evidence.Row) int64 { return r.Time.UnixMilli() }

// Repos returns the repo mapping at h from rows: of the repo rows with a
// sequence number at or below h, the one with the highest per exact path,
// sorted by path. Within the block whose range holds h, the repo bucket
// holds that mapping (ADR 0019 point 4). The engine resolves equal paths
// by project name, not by sequence, so the reduction happens here.
func Repos(rows []evidence.Row, h int64) []struggle.Repo {
	type latest struct {
		seq  int64
		repo evidence.Repo
	}
	byPath := map[string]latest{}
	for _, r := range rows {
		if r.Type != evidence.TypeRepo || r.Seq <= 0 || r.Seq > h {
			continue
		}
		v, ok := r.Value.(evidence.Repo)
		if !ok {
			continue
		}
		if cur, seen := byPath[v.Path]; !seen || r.Seq > cur.seq {
			byPath[v.Path] = latest{seq: r.Seq, repo: v}
		}
	}
	out := make([]struggle.Repo, 0, len(byPath))
	for _, l := range byPath {
		out = append(out, struggle.Repo{Path: l.repo.Path, Project: l.repo.Project})
	}
	slices.SortFunc(out, func(a, b struggle.Repo) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// engineHeartbeat is the part of a heartbeat the engine reads.
func engineHeartbeat(h wire.Heartbeat) struggle.Heartbeat {
	return struggle.Heartbeat{
		Time: h.Timestamp(), Project: h.Project, Entity: h.Entity, IsWrite: h.IsWrite,
		Lineno: h.Lineno, Cursorpos: h.Cursorpos, AI: isAIHeartbeat(h),
	}
}

// isAIHeartbeat reports whether the editor attributed the heartbeat to an
// AI tool: category "ai coding" or any of Hackatime's ai_* fields.
func isAIHeartbeat(h wire.Heartbeat) bool {
	return strings.EqualFold(h.Category, "ai coding") || h.AIModel != "" || h.AISession != "" ||
		h.AIInputTokens != nil || h.AIOutputTokens != nil || h.AIPromptLength != nil || h.AILineChanges != nil
}
