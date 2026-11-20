package crediting

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Window arithmetic (ADR 0010, ADR 0016).
const (
	// Window is how far back each run recomputes minutes.
	Window = 60 * time.Minute
	// History is the extra context before the window the engine reads: the
	// engine's whole look-back, so every minute in the window, also one
	// finalized at the window's start after a restart, is computed from
	// all the events its rules read.
	History = struggle.ContextBefore
	// FinalAfter is the age at which the server takes no more rewrites of
	// a minute it has seen.
	FinalAfter = 30 * time.Minute
	// FinalLead is when a minute becomes final on the agent: the first
	// settled run at or after minute + FinalLead computes it for the last
	// time, with that run's arrival cutoff, while the server still takes
	// the rewrite. It is past struggle.Lookahead, so the minute's rules have
	// all the events after it they read.
	FinalLead = FinalAfter - time.Minute
	// DefaultSettle is how long after a start (or a gap in runs, such as a
	// sleep) the agent waits before making minutes final, so events that
	// arrive late because it was down (the hook spool, wakatime-cli's
	// offline queue) still count.
	DefaultSettle = 15 * time.Minute
	// DayGrace is how long after a UTC day ends before it is sealed.
	DayGrace = time.Hour
)

// span is one run's time ranges, all UTC.
type span struct {
	// from and to bound the minutes the engine emits, [from, to).
	from, to time.Time
	// history is where event reads start.
	history time.Time
	// finalBefore: minutes before it are final.
	finalBefore time.Time
}

// spanAt returns the ranges for a run at now. The minute now is in hasn't
// ended, so it is not computed yet.
func spanAt(now time.Time) span {
	to := now.UTC().Truncate(time.Minute)
	from := to.Add(-Window)
	return span{from: from, to: to, history: from.Add(-History), finalBefore: finalBefore(now)}
}

// finalBefore returns the first minute that is not yet final at now: a
// minute M is final once now >= M + FinalAfter.
func finalBefore(now time.Time) time.Time {
	return now.UTC().Add(-FinalAfter).Truncate(time.Minute).Add(time.Minute)
}

// cutoff is a run's arrival cutoff: the end of its window. The run reads
// the events stored by then; one stored later counts from the next run.
// It is a whole minute, a few seconds before the run starts, so every row
// stamped by then is committed when the run reads (rows are stamped while
// their transaction holds the write lock).
func (s span) cutoff() time.Time { return s.to }

// engineInput builds the engine's Input for one run from local events: those
// in [s.history, s.to) that arrived by the run's cutoff.
func engineInput(s span, ev localdb.Events, active []struggle.ActiveTime, repos []struggle.Repo, tz *time.Location) struggle.Input {
	cutoff := s.cutoff()
	var hbs []struggle.Heartbeat
	for _, h := range arrivedBy(ev.Heartbeats, s, cutoff, heartbeatTime) {
		hbs = append(hbs, engineHeartbeat(h))
	}
	return struggle.Input{
		Window:     struggle.Window{From: s.from, To: s.to},
		Activity:   arrivedBy(ev.Activity, s, cutoff, activityTime),
		Heartbeats: hbs,
		AI:         arrivedBy(ev.AI, s, cutoff, aiTime),
		ActiveTime: active,
		Repos:      repos,
		TZ:         tz,
	}
}

// arrivedBy returns the events of s that arrived by cutoff, with time in
// [s.history, s.to).
func arrivedBy[T any](evs []evidence.Received[T], s span, cutoff time.Time,
	at func(evidence.Received[T]) time.Time) []T {
	out := make([]T, 0, len(evs))
	for _, e := range evs {
		if t := at(e); !t.Before(s.history) && t.Before(s.to) && !e.Arrived.After(cutoff) {
			out = append(out, e.Event)
		}
	}
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

// minuteRows turns the engine's result into the rows to store, sorted by
// minute and project. Minutes outside [s.from, s.to) and repeats of a
// (minute, project) pair are dropped. So is an idle minute with no project,
// no credit and no flags: it says only "nothing happened", which is what a
// missing minute means too, and uploading it would cost the server a row
// per device per minute.
func minuteRows(s span, res struggle.Result) []localdb.CreditedMinute {
	out := make([]localdb.CreditedMinute, 0, len(res.Minutes))
	for _, m := range res.Minutes {
		at := m.Minute.UTC().Truncate(time.Minute)
		if at.Before(s.from) || !at.Before(s.to) {
			continue
		}
		if m.Label == struggle.LabelIdle && m.Project == "" && m.Credited == 0 && len(m.Flags) == 0 {
			continue
		}
		out = append(out, newRow(at, m.Project, string(m.Label), m.Credited, m.Cuts, m.Flags, res.RulesVersion))
	}
	slices.SortStableFunc(out, func(a, b localdb.CreditedMinute) int {
		if c := a.Minute.Compare(b.Minute); c != 0 {
			return c
		}
		return cmp.Compare(a.Project, b.Project)
	})
	return slices.CompactFunc(out, func(a, b localdb.CreditedMinute) bool {
		return a.Minute.Equal(b.Minute) && a.Project == b.Project
	})
}

// superseded is the row that replaces an open minute's project the engine no
// longer reports, once the server has seen it: labelled idle, with no
// credit, cuts or flags. The server reads that shape as a removal and
// deletes its row (credited.IsRemoval, ADR 0014), which frees the project's
// slot for the one that displaced it; both go in one batch, since a minute's
// rows are never split, and the removal sorts first (no credit).
func superseded(old localdb.CreditedMinute) localdb.CreditedMinute {
	return newRow(old.Minute, old.Project, string(struggle.LabelIdle), 0, nil, nil, struggle.RulesVersion)
}

func newRow(at time.Time, project, label string, credited int, cuts, flags []string, rules string) localdb.CreditedMinute {
	r := localdb.CreditedMinute{
		Minute: at, Project: project, Label: label, Credited: credited,
		Cuts: sortedCopy(cuts), Flags: sortedCopy(flags), RulesVersion: rules,
	}
	r.Digest = digest(r)
	return r
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	slices.Sort(out)
	return out
}

// digest identifies a row's uploadable content.
func digest(r localdb.CreditedMinute) string {
	h := sha256.New()
	for _, part := range []string{r.Label, strconv.Itoa(r.Credited), strings.Join(r.Cuts, ","),
		strings.Join(r.Flags, ","), r.RulesVersion} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// evidenceActiveTimes converts active-time datapoints for the evidence
// encoding, keeping those whose To falls in [start, end).
func evidenceActiveTimes(in []struggle.ActiveTime, start, end time.Time) []evidence.ActiveTime {
	var out []evidence.ActiveTime
	for _, a := range in {
		if a.To.Before(start) || !a.To.Before(end) {
			continue
		}
		out = append(out, evidence.ActiveTime{
			From: a.From, To: a.To, SessionID: a.SessionID, UserSecs: a.UserSecs, CLISecs: a.CLISecs,
			Harness: a.Harness, ProjectCwd: a.ProjectCwd,
		})
	}
	return out
}

// utcDay returns the UTC date of t.
func utcDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// lastSealableDay returns the latest UTC day that has ended at least
// DayGrace before now.
func lastSealableDay(now time.Time) string {
	t := now.UTC().Add(-DayGrace)
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return utcDay(midnight.AddDate(0, 0, -1))
}

// nextDay returns the UTC day after day (YYYY-MM-DD).
func nextDay(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day // callers pass days they formatted; unreachable
	}
	return utcDay(t.AddDate(0, 0, 1))
}
