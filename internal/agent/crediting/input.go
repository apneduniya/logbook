package crediting

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Window arithmetic (ADR 0010, ADR 0017).
const (
	// Window is how far back each run recomputes minutes.
	Window = replay.Window
	// History is the extra context before the window the engine reads: the
	// engine's whole look-back, so every minute in the window is computed
	// from all the events its rules read.
	History = replay.History
	// FinalAfter is the age at which the server takes no more rewrites of
	// a minute it has seen.
	FinalAfter = 30 * time.Minute
	// FinalLead is when a minute becomes final on the agent: its first
	// settled run with T >= minute + FinalLead makes it final, while the
	// server still takes the write. It is past struggle.Lookahead, so the
	// minute's rules have all the events after it they read.
	FinalLead = FinalAfter - time.Minute
	// DefaultSettle is how long after a start (or a wake, or a long gap in
	// runs) the agent waits before making minutes final, so events that
	// arrive late because it was down (the hook spool, wakatime-cli's
	// offline queue, a delayed OTLP export) still count.
	DefaultSettle = 15 * time.Minute
	// DayGrace is how long after a UTC day ends before it is sealed.
	DayGrace = time.Hour
)

// span is one run's time ranges, all UTC.
type span struct {
	// from and to bound the minutes the engine emits, [from, to). to is the
	// run's T: the end of its window, a whole minute.
	from, to time.Time
	// history is where event reads start.
	history time.Time
}

// spanAt returns the ranges of the run whose window ends at T, the whole
// minute now is in (that minute hasn't ended, so it isn't computed yet).
func spanAt(now time.Time) span {
	s := replay.SpanAt(now)
	return span{from: s.From, to: s.To, history: s.History}
}

// finalBefore returns where a settled run at T makes minutes final: every
// minute m with T >= m + FinalLead, so every minute before T - FinalLead
// plus one minute.
func finalBefore(t time.Time) time.Time {
	return t.UTC().Truncate(time.Minute).Add(-FinalLead).Add(time.Minute)
}

// serverFinalBefore returns the first minute the server doesn't hold final
// yet at now: a minute M is final there once now >= M + FinalAfter.
func serverFinalBefore(now time.Time) time.Time {
	return now.UTC().Add(-FinalAfter).Truncate(time.Minute).Add(time.Minute)
}

// engineInput builds the engine's Input for the run at key from the
// logged events of its snapshot (all at or below key.H) and the repo
// mapping at key.H, with pkg/replay: the same code a replay of its minutes
// runs, here or on the server. Claude Code active time is added after:
// replays leave it out, since it feeds only flags (ADR 0019 point 8).
func engineInput(key localdb.Key, ev localdb.Events, active []struggle.ActiveTime, repos []struggle.Repo, tz *time.Location) (struggle.Input, error) {
	in, err := replay.InputWithRepos(eventRows(ev), repos, replay.Key{T: key.T, H: key.H}, tz)
	if err != nil {
		return struggle.Input{}, fmt.Errorf("building the engine input: %w", err)
	}
	in.ActiveTime = active
	return in, nil
}

// eventRows are the engine's logged events as evidence rows. Git events
// are left out: the engine never reads them.
func eventRows(ev localdb.Events) []evidence.Row {
	out := make([]evidence.Row, 0, len(ev.Activity)+len(ev.Heartbeats)+len(ev.AI))
	for _, e := range ev.Activity {
		out = append(out, evidence.ActivityRow(e.Seq, e.Event))
	}
	for _, e := range ev.Heartbeats {
		out = append(out, evidence.HeartbeatRow(e.Seq, e.Event))
	}
	for _, e := range ev.AI {
		out = append(out, evidence.AIRow(e.Seq, e.Event))
	}
	return out
}

// Event times as stored and ordered by the local database: unix
// milliseconds. A heartbeat's payload may carry finer times, which the
// database orders by sequence number within the millisecond.
func activityTime(e wire.ActivitySample) time.Time { return e.Time }
func aiTime(e wire.AIEvent) time.Time              { return e.Time }
func gitTime(e wire.GitEvent) time.Time            { return e.Time }
func heartbeatTime(h wire.Heartbeat) time.Time {
	return time.UnixMilli(h.Timestamp().UnixMilli()).UTC()
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

// removal is the row that removes a project the server holds from an open
// minute whose new result no longer has it: labelled idle, with no credit,
// cuts or flags. The server reads that shape as a removal and deletes its
// row (credited.IsRemoval, ADR 0014), which frees the project's slot for the
// one that displaced it; both go in one batch, since a minute's rows are
// never split, and the removal sorts first (no credit). The engine never
// emits that shape.
func removal(at time.Time, project string) localdb.CreditedMinute {
	return newRow(at, project, string(struggle.LabelIdle), 0, nil, nil, struggle.RulesVersion)
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
