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

// Window arithmetic (ADR 0010).
const (
	// Window is how far back each run recomputes minutes.
	Window = 60 * time.Minute
	// History is the extra context before the window the engine reads.
	History = 30 * time.Minute
	// FinalAfter is the age at which a minute becomes final.
	FinalAfter = 30 * time.Minute
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

// engineInput builds the engine's Input for one run from local events.
func engineInput(s span, ev localdb.Events, active []struggle.ActiveTime, repos []struggle.Repo, tz *time.Location) struggle.Input {
	hbs := make([]struggle.Heartbeat, len(ev.Heartbeats))
	for i, h := range ev.Heartbeats {
		hbs[i] = engineHeartbeat(h)
	}
	return struggle.Input{
		Window:     struggle.Window{From: s.from, To: s.to},
		Activity:   ev.Activity,
		Heartbeats: hbs,
		AI:         ev.AI,
		ActiveTime: active,
		Repos:      repos,
		TZ:         tz,
	}
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
// longer reports, once the server has seen it: no credit, labelled idle.
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
