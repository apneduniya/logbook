package submit

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/projectname"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Why one of this device's minutes isn't listed in its part, shown in the
// summary. The server marks such minutes unbacked.
const (
	ReasonNoKey  = "computed before minutes recorded how they were computed"
	ReasonKeys   = "its rows were computed at different keys"
	ReasonBadKey = "its key's window doesn't hold it"
	// ReasonUnproven covers every key crediting.BuildProofs leaves
	// unbacked.
	ReasonUnproven = "its evidence can't be proven from this device's log: the day isn't sealed and accepted yet, " +
		"the run read rows outside the provable blocks, or rows it needs were deleted"
)

// Prepared is this device's part of a submission and what it reveals.
type Prepared struct {
	Part    submission.Part
	Summary Summary
}

// Summary is what the user is shown before anything is uploaded (ADR 0020
// step 4).
type Summary struct {
	// Minutes are the distinct minutes of this device the part backs, and
	// Credited how many of them have credit.
	Minutes, Credited int
	// Days are the UTC days of those minutes, and Blocks the days of the
	// sealed blocks the part proves, both in order.
	Days, Blocks []string
	// Revealed is what the opened buckets show.
	Revealed Revealed
	// Unbacked are this device's minutes the part can't back, by reason.
	Unbacked []Count
	// Others are the minutes of the plan's other devices, which have no
	// evidence until each adds its part.
	Others []Count
}

// Count is a number of minutes with what they share (a reason or a
// device name).
type Count struct {
	What    string
	Minutes int
}

// Revealed is what the server learns from the part's opened buckets
// beyond labelled minutes: every row in the replay windows, of any
// project, and the repo folders of each opened repo bucket.
type Revealed struct {
	// Project are the rows of the submitted project.
	Project ProjectRows
	// Others are the rows attributed to other projects ("" for events
	// with no project), by name.
	Others []ProjectRows
	// Activity counts activity samples (idle seconds and the focused
	// app's name) and ActiveTime Claude Code active-time rows: neither
	// belongs to a project.
	Activity, ActiveTime int
	// Repos are the repo folders and their projects, by path.
	Repos []evidence.Repo
}

// ProjectRows counts one project's revealed rows by kind.
type ProjectRows struct {
	Project string
	// Git counts git events: commits and working-tree diff sizes.
	Heartbeats, AIEvents, Git int
}

// Total is the project's number of rows.
func (p ProjectRows) Total() int { return p.Heartbeats + p.AIEvents + p.Git }

// Keys returns the distinct computation keys of the device's minutes in
// plan that a part could list: what crediting.BuildProofs is asked to
// prove. Minutes Prepare would leave out for their rows alone add no key,
// so their windows are never opened.
func Keys(plan submission.Plan, deviceID string) []replay.Key {
	var out []replay.Key
	for at, rows := range ownMinutes(plan, deviceID) {
		if k, reason := minuteKey(at, rows, nil); reason == "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b replay.Key) int { return cmp.Or(a.T.Compare(b.T), cmp.Compare(a.H, b.H)) })
	return out
}

// Prepare builds the device's part from the plan and the proofs built for
// Keys(plan, deviceID). A minute is listed when all its rows carry one
// valid key, its key's window holds it, and the proofs back the key;
// otherwise its reason is counted in the summary.
func Prepare(plan submission.Plan, deviceID string, proofs crediting.Proofs) (Prepared, error) {
	unproven := map[replay.Key]bool{}
	for _, k := range proofs.Unbacked {
		unproven[replay.Key{T: k.T.UTC(), H: k.H}] = true
	}
	byMinute := ownMinutes(plan, deviceID)
	others := map[string]map[time.Time]bool{}
	for _, m := range plan.Minutes {
		if m.DeviceID == deviceID {
			continue
		}
		if others[m.DeviceID] == nil {
			others[m.DeviceID] = map[time.Time]bool{}
		}
		others[m.DeviceID][m.Minute.UTC()] = true
	}

	p := Prepared{Part: submission.Part{DeviceID: deviceID, Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}}}
	reasons := map[string]int{}
	days := map[string]bool{}
	var maxH int64
	for _, at := range slices.SortedFunc(maps.Keys(byMinute), time.Time.Compare) {
		key, reason := minuteKey(at, byMinute[at], unproven)
		if reason != "" {
			reasons[reason]++
			continue
		}
		p.Part.Minutes = append(p.Part.Minutes, submission.PartMinute{Minute: at, ComputedAt: key.T, Snapshot: key.H})
		p.Summary.Minutes++
		if slices.ContainsFunc(byMinute[at], func(m submission.Minute) bool { return m.Credited > 0 }) {
			p.Summary.Credited++
		}
		days[at.Format(time.DateOnly)] = true
		maxH = max(maxH, key.H)
	}
	p.Summary.Days = slices.Sorted(maps.Keys(days))
	for _, r := range slices.Sorted(maps.Keys(reasons)) {
		p.Summary.Unbacked = append(p.Summary.Unbacked, Count{What: r, Minutes: reasons[r]})
	}
	for _, d := range plan.Devices {
		if n := len(others[d.ID]); n > 0 {
			p.Summary.Others = append(p.Summary.Others, Count{What: cmp.Or(d.Name, d.ID), Minutes: n})
		}
		delete(others, d.ID)
	}
	for _, id := range slices.Sorted(maps.Keys(others)) { // rows of a device the plan doesn't list
		p.Summary.Others = append(p.Summary.Others, Count{What: id, Minutes: len(others[id])})
	}

	if len(p.Part.Minutes) > 0 {
		var rows []evidence.Row
		for _, b := range proofs.Blocks {
			p.Part.Blocks = append(p.Part.Blocks, submission.Block{Day: b.Record.Day, Proof: b.Proof})
			p.Summary.Blocks = append(p.Summary.Blocks, b.Record.Day)
			r, err := openedRows(b.Proof)
			if err != nil {
				return Prepared{}, fmt.Errorf("reading the proof of %s: %w", b.Record.Day, err)
			}
			rows = append(rows, r...)
		}
		p.Summary.Revealed = reveal(plan.Project, rows, maxH)
	}
	if err := p.Part.Validate(); err != nil {
		return Prepared{}, fmt.Errorf("building this device's part: %w", err)
	}
	return p, nil
}

// ownMinutes returns the device's rows in plan by minute (UTC).
func ownMinutes(plan submission.Plan, deviceID string) map[time.Time][]submission.Minute {
	out := map[time.Time][]submission.Minute{}
	for _, m := range plan.Minutes {
		if m.DeviceID == deviceID {
			out[m.Minute.UTC()] = append(out[m.Minute.UTC()], m)
		}
	}
	return out
}

// minuteKey returns the key a minute's rows share, or why the minute
// can't be listed.
func minuteKey(at time.Time, rows []submission.Minute, unproven map[replay.Key]bool) (replay.Key, string) {
	var key replay.Key
	for i, r := range rows {
		k, ok := r.Key()
		if !ok {
			return replay.Key{}, ReasonNoKey
		}
		if i > 0 && (!k.T.Equal(key.T) || k.H != key.H) {
			return replay.Key{}, ReasonKeys
		}
		key = k
	}
	if key.Validate() != nil || at.Before(key.T.Add(-replay.Window)) || !at.Before(key.T) {
		return replay.Key{}, ReasonBadKey
	}
	if unproven[key] {
		return replay.Key{}, ReasonUnproven
	}
	return key, ""
}

// openedRows decodes the rows of a proof's opened buckets.
func openedRows(p evidence.Proof) ([]evidence.Row, error) {
	var out []evidence.Row
	for _, b := range p.Opened {
		for _, l := range b.Lines {
			r, err := evidence.DecodeLine([]byte(l))
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// reveal summarises revealed rows. Events are attributed to projects as
// the engine does (struggle.RepoMap), with the repo mapping at h, the
// highest key the part backs; git events by their own project name.
func reveal(project string, rows []evidence.Row, h int64) Revealed {
	var repos []evidence.Row
	for _, r := range rows {
		if r.Type == evidence.TypeRepo {
			repos = append(repos, r)
		}
	}
	mapping := replay.Repos(repos, h)
	rm := struggle.NewRepoMap(mapping)
	var out Revealed
	byPath := map[string]evidence.Repo{}
	for _, r := range repos {
		if v, ok := r.Value.(evidence.Repo); ok {
			byPath[v.Path] = v
		}
	}
	// The mapping at h wins over older rows of the same path.
	for _, m := range mapping {
		byPath[m.Path] = evidence.Repo{Path: m.Path, Project: m.Project}
	}
	for _, p := range slices.Sorted(maps.Keys(byPath)) {
		out.Repos = append(out.Repos, byPath[p])
	}

	counts := map[string]*ProjectRows{}
	row := func(name string) *ProjectRows {
		if counts[name] == nil {
			counts[name] = &ProjectRows{Project: name}
		}
		return counts[name]
	}
	for _, r := range rows {
		switch v := r.Value.(type) {
		case wire.Heartbeat:
			row(rm.Project(v.Entity, v.Project)).Heartbeats++
		case wire.AIEvent:
			row(rm.Project(v.Cwd, v.Project)).AIEvents++
		case wire.GitEvent:
			row(projectname.Normalize(v.Project)).Git++
		case wire.ActivitySample:
			out.Activity++
		case evidence.ActiveTime:
			out.ActiveTime++
		}
	}
	if c := counts[project]; c != nil {
		out.Project = *c
	}
	out.Project.Project = project
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		if name != project {
			out.Others = append(out.Others, *counts[name])
		}
	}
	return out
}
