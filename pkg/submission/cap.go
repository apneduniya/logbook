package submission

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// CapInput is one credited row of the user, of any project and device,
// frozen at submission (submission_cap_inputs) so the cap can be
// recomputed offline.
type CapInput struct {
	DeviceID string    `json:"device_id"`
	Minute   time.Time `json:"minute"`
	Project  string    `json:"project"`
	Credited int       `json:"credited"`
}

// CappedMinute is the project's capped seconds in one minute
// (submission_capped).
type CappedMinute struct {
	Minute  time.Time `json:"minute"`
	Seconds int64     `json:"seconds"`
}

// CappedProject is one row of the credited_project_minutes view: a
// project's capped seconds in one minute.
type CappedProject struct {
	Minute  time.Time `json:"minute"`
	Project string    `json:"project"`
	Seconds int64     `json:"seconds"`
}

// Cap computes, from one user's credited rows, the capped seconds of each
// (minute, project) exactly as the credited_project_minutes view does
// (migration 00003, ADR 0014): a project's credit is summed over devices;
// when the minute's total over every project and device is at most 60,
// the project keeps its sum, otherwise it gets sum * 60 / total, rounded
// down (integer division). Rows are sorted by (minute, project).
func Cap(inputs []CapInput) []CappedProject {
	type mp struct {
		minute  int64 // Unix seconds, so equal instants share a key
		project string
	}
	sums := map[mp]int64{}
	totals := map[int64]int64{}
	for _, in := range inputs {
		m := in.Minute.Unix()
		sums[mp{m, in.Project}] += int64(in.Credited)
		totals[m] += int64(in.Credited)
	}
	out := make([]CappedProject, 0, len(sums))
	for k, sum := range sums {
		secs := sum
		if total := totals[k.minute]; total > 60 {
			secs = sum * 60 / total
		}
		out = append(out, CappedProject{Minute: time.Unix(k.minute, 0).UTC(), Project: k.project, Seconds: secs})
	}
	slices.SortFunc(out, func(a, b CappedProject) int {
		return cmp.Or(a.Minute.Compare(b.Minute), cmp.Compare(a.Project, b.Project))
	})
	return out
}

// CapProject returns Cap(inputs) for one project, by minute: what the
// submission freezes as its capped seconds.
func CapProject(inputs []CapInput, project string) []CappedMinute {
	var out []CappedMinute
	for _, c := range Cap(inputs) {
		if c.Project == project {
			out = append(out, CappedMinute{Minute: c.Minute, Seconds: c.Seconds})
		}
	}
	return out
}

// ErrCapMismatch is wrapped when frozen capped seconds aren't what the
// frozen cap inputs give.
var ErrCapMismatch = fmt.Errorf("%w: capped seconds", ErrMalformed)

// CheckCap checks a submission's frozen cap: every frozen row of the
// project is among the cap inputs with the same credit, and the frozen
// capped seconds are exactly CapProject(inputs, project), minute for
// minute.
func CheckCap(project string, rows []Minute, capped []CappedMinute, inputs []CapInput) error {
	type dm struct {
		device  string
		minute  int64
		project string
	}
	in := make(map[dm]int, len(inputs))
	for _, c := range inputs {
		k := dm{c.DeviceID, c.Minute.Unix(), c.Project}
		if _, dup := in[k]; dup {
			return fmt.Errorf("%w: two cap inputs for %s %s %q", ErrCapMismatch, c.DeviceID, c.Minute.UTC().Format(time.RFC3339), c.Project)
		}
		in[k] = c.Credited
	}
	for _, r := range rows {
		if r.Project != project {
			return fmt.Errorf("%w: a frozen row of project %q", ErrCapMismatch, r.Project)
		}
		if got, ok := in[dm{r.DeviceID, r.Minute.Unix(), r.Project}]; !ok || got != r.Credited {
			return fmt.Errorf("%w: the row of %s at %s isn't among the cap inputs", ErrCapMismatch,
				r.DeviceID, r.Minute.UTC().Format(time.RFC3339))
		}
	}
	want := CapProject(inputs, project)
	got := slices.Clone(capped)
	slices.SortFunc(got, func(a, b CappedMinute) int { return a.Minute.Compare(b.Minute) })
	for i := range max(len(want), len(got)) {
		switch {
		case i >= len(got):
			return fmt.Errorf("%w: %s is missing", ErrCapMismatch, want[i].Minute.Format(time.RFC3339))
		case i >= len(want):
			return fmt.Errorf("%w: %s has no cap inputs", ErrCapMismatch, got[i].Minute.UTC().Format(time.RFC3339))
		case !got[i].Minute.Equal(want[i].Minute):
			return fmt.Errorf("%w: frozen %s, recomputed %s", ErrCapMismatch,
				got[i].Minute.UTC().Format(time.RFC3339), want[i].Minute.Format(time.RFC3339))
		case got[i].Seconds != want[i].Seconds:
			return fmt.Errorf("%w: %s frozen %d s, recomputed %d s", ErrCapMismatch,
				want[i].Minute.Format(time.RFC3339), got[i].Seconds, want[i].Seconds)
		}
	}
	return nil
}
