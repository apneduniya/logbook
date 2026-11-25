package submission

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// turnGap is how long a session's turn lasts without events: the
// engine's turn timeout (docs/STRUGGLE-RULES.md).
const turnGap = 10 * time.Minute

// kind is what a counted minute is made of (MadeOf).
type kind int

const (
	kindOther kind = iota
	kindWithAI
	kindEditing
)

// activity reads the revealed rows of every device for the packet. It
// attributes events to projects exactly as the engine does
// (struggle.RepoMap), with the repo mapping at the sequence number in
// question. It is display only: nothing here changes credit.
type activity struct {
	project string
	devs    map[string]*deviceLog
}

// deviceLog is one device's revealed rows.
type deviceLog struct {
	// events are the heartbeat, AI and git rows, by (time, seq).
	events []evidence.Row
	repos  []evidence.Row
	maps   map[int64]struggle.RepoMap
	// maxH is the highest key of the device's backed rows, 0 when none.
	maxH int64
}

func newActivity(project string, revealed []DeviceRows, results []RowResult) *activity {
	a := &activity{project: project, devs: map[string]*deviceLog{}}
	for _, d := range revealed {
		dl := a.devs[d.DeviceID]
		if dl == nil {
			dl = &deviceLog{maps: map[int64]struggle.RepoMap{}}
			a.devs[d.DeviceID] = dl
		}
		for _, r := range d.Rows {
			switch r.Type {
			case evidence.TypeRepo:
				dl.repos = append(dl.repos, r)
			case evidence.TypeHeartbeat, evidence.TypeAI, evidence.TypeGit:
				dl.events = append(dl.events, r)
			}
		}
	}
	for _, dl := range a.devs {
		slices.SortFunc(dl.events, func(x, y evidence.Row) int { return cmp.Or(x.Time.Compare(y.Time), cmp.Compare(x.Seq, y.Seq)) })
	}
	for _, r := range results {
		if k, ok := r.Row.Key(); ok && r.Result != ResultUnbacked {
			if dl := a.devs[r.Row.DeviceID]; dl != nil && k.H > dl.maxH {
				dl.maxH = k.H
			}
		}
	}
	return a
}

// repoMap is the device's repo mapping at h.
func (d *deviceLog) repoMap(h int64) struggle.RepoMap {
	m, ok := d.maps[h]
	if !ok {
		m = struggle.NewRepoMap(replay.Repos(d.repos, h))
		d.maps[h] = m
	}
	return m
}

// between returns the device's events with a time in [from, to).
func (d *deviceLog) between(from, to time.Time) []evidence.Row {
	lo := sort.Search(len(d.events), func(i int) bool { return !d.events[i].Time.Before(from) })
	hi := sort.Search(len(d.events), func(i int) bool { return !d.events[i].Time.Before(to) })
	return d.events[lo:hi]
}

// madeOf classifies a counted minute by the revealed rows of its backed
// rows (MadeOf): editing when a human heartbeat of the project is in the
// minute; else with AI when an AI event of the project is in the minute
// (a prompt, a permission answer, or the agent working while the person
// watches) or a turn of a session of the project is open into it (its
// last event before the minute is under 10 minutes earlier and doesn't
// end the turn); else other. Each row's events are those with seq <= its
// key's H, attributed with the mapping at H. Unbacked rows reveal
// nothing.
func (a *activity) madeOf(rows []RowResult) kind {
	best := kindOther
	for _, r := range rows {
		k, ok := r.Row.Key()
		dl := a.devs[r.Row.DeviceID]
		if !ok || dl == nil || r.Result == ResultUnbacked {
			continue
		}
		rm := dl.repoMap(k.H)
		m := r.Row.Minute.UTC()
		last := map[string]evidence.Row{} // each session's last event before the minute
		for _, e := range dl.between(m.Add(-turnGap), m.Add(time.Minute)) {
			if e.Seq > k.H {
				continue
			}
			in := !e.Time.Before(m)
			switch v := e.Value.(type) {
			case wire.Heartbeat:
				if in && !replay.AIHeartbeat(v) && rm.Project(v.Entity, v.Project) == a.project {
					return kindEditing
				}
			case wire.AIEvent:
				if rm.Project(v.Cwd, v.Project) != a.project {
					continue
				}
				if in {
					best = kindWithAI
				} else {
					last[string(v.Harness)+"/"+v.SessionID] = e
				}
			}
		}
		for _, e := range last {
			if v, ok := e.Value.(wire.AIEvent); ok && !endsTurn(v.Kind) {
				best = kindWithAI
			}
		}
	}
	return best
}

func endsTurn(k wire.AIEventKind) bool { return k == wire.AITurnEnd || k == wire.AISessionEnd }

// sessions summarises the project's AI sessions with an event in the
// range, from each device's revealed AI events attributed with the
// mapping at the device's highest backed key. A session's minutes are
// the minutes of its events, and every minute from an event that doesn't
// end a turn to the session's next event when that is at most 10 minutes
// later. Sessions are sorted by start.
func (a *activity) sessions(rg Range, capped map[int64]int64, ms map[int64][]RowResult) []AISession {
	out := []AISession{}
	for _, id := range slices.Sorted(maps.Keys(a.devs)) {
		dl := a.devs[id]
		if dl.maxH == 0 {
			continue
		}
		rm := dl.repoMap(dl.maxH)
		bySession := map[string][]wire.AIEvent{}
		for _, e := range dl.events {
			if v, ok := e.Value.(wire.AIEvent); ok && rm.Project(v.Cwd, v.Project) == a.project {
				k := string(v.Harness) + "/" + v.SessionID
				bySession[k] = append(bySession[k], v)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(bySession)) {
			if s, ok := session(bySession[k], rg, capped, ms); ok {
				out = append(out, s)
			}
		}
	}
	slices.SortStableFunc(out, func(x, y AISession) int {
		return cmp.Or(x.From.Compare(y.From), cmp.Compare(x.Harness, y.Harness), x.To.Compare(y.To))
	})
	return out
}

// session summarises one session's events (by time); ok is false when
// none is in the range.
func session(evs []wire.AIEvent, rg Range, capped map[int64]int64, ms map[int64][]RowResult) (AISession, bool) {
	if !slices.ContainsFunc(evs, func(e wire.AIEvent) bool { return rg.Contains(e.Time) }) {
		return AISession{}, false
	}
	s := AISession{Harness: string(evs[0].Harness), From: evs[0].Time.UTC(), To: evs[len(evs)-1].Time.UTC()}
	minutes := map[int64]bool{}
	for i, e := range evs {
		if e.Model != "" {
			s.Model = e.Model
		}
		switch e.Kind {
		case wire.AIPromptSubmit:
			s.Prompts++
		case wire.AITurnEnd:
			s.Turns++
		}
		from, to := e.Time.Truncate(time.Minute), e.Time.Truncate(time.Minute)
		if i+1 < len(evs) && !endsTurn(e.Kind) && evs[i+1].Time.Sub(e.Time) <= turnGap {
			to = evs[i+1].Time.Truncate(time.Minute)
		}
		for m := from; !m.After(to); m = m.Add(time.Minute) {
			minutes[m.Unix()] = true
		}
	}
	for m := range minutes {
		s.CountedSeconds += capped[m]
		if slices.ContainsFunc(ms[m], func(r RowResult) bool { return r.Row.Label == string(struggle.LabelAgent) }) {
			s.AgentAloneSeconds += 60
		}
	}
	return s, true
}

// commits returns the project's commits in the range by day: revealed git
// commit events whose project is the submission's, once per hash (the
// earliest), by time.
func (a *activity) commits(rg Range, repoURL string) map[string][]Commit {
	seen := map[string]wire.GitEvent{}
	for _, dl := range a.devs {
		for _, e := range dl.events {
			g, ok := e.Value.(wire.GitEvent)
			if !ok || g.Kind != "commit" || g.CommitHash == "" || normalProject(g.Project) != a.project || !rg.Contains(g.Time) {
				continue
			}
			if cur, dup := seen[g.CommitHash]; !dup || g.Time.Before(cur.Time) {
				seen[g.CommitHash] = g
			}
		}
	}
	byDay := map[string][]Commit{}
	for _, hash := range slices.Sorted(maps.Keys(seen)) {
		g := seen[hash]
		c := Commit{At: g.Time.UTC(), Hash: hash[:min(7, len(hash))], Files: g.FilesChanged, Added: g.Insertions,
			Removed: g.Deletions, URL: commitURL(repoURL, hash)}
		day := c.At.Format(time.DateOnly)
		byDay[day] = append(byDay[day], c)
	}
	for _, cs := range byDay {
		slices.SortStableFunc(cs, func(x, y Commit) int { return x.At.Compare(y.At) })
	}
	return byDay
}
