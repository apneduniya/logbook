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

// CheckCap checks a submission's frozen cap against its frozen rows, so
// the capped seconds of a minute never exceed what the frozen rows of the
// project credit in it:
//   - every frozen row of the project is unique and among the cap inputs
//     with the same credit;
//   - every cap input lies at a minute of a frozen row, credits 0 to 60 s
//     (as credited_minutes does), and when it is of the project, it is a
//     frozen row: the project's credit in a minute comes only from frozen
//     rows, each verified, mismatched or unbacked on its own;
//   - the frozen capped seconds are exactly CapProject(inputs, project),
//     minute for minute, so every capped minute has a frozen row.
func CheckCap(project string, rows []Minute, capped []CappedMinute, inputs []CapInput) error {
	type dm struct {
		device string
		minute int64
	}
	frozen := make(map[dm]int, len(rows))
	for _, r := range rows {
		if r.Project != project {
			return fmt.Errorf("%w: a frozen row of project %q", ErrCapMismatch, r.Project)
		}
		k := dm{r.DeviceID, r.Minute.Unix()}
		if _, dup := frozen[k]; dup {
			return fmt.Errorf("%w: two frozen rows of %s at %s", ErrCapMismatch, r.DeviceID, r.Minute.UTC().Format(time.RFC3339))
		}
		frozen[k] = r.Credited
	}
	minutes := make(map[int64]bool, len(rows))
	for k := range frozen {
		minutes[k.minute] = true
	}
	type dmp struct {
		dm
		project string
	}
	in := make(map[dmp]int, len(inputs))
	for _, c := range inputs {
		at := c.Minute.UTC().Format(time.RFC3339)
		k := dmp{dm{c.DeviceID, c.Minute.Unix()}, c.Project}
		if _, dup := in[k]; dup {
			return fmt.Errorf("%w: two cap inputs for %s %s %q", ErrCapMismatch, c.DeviceID, at, c.Project)
		}
		in[k] = c.Credited
		switch {
		case c.Credited < 0 || c.Credited > 60:
			return fmt.Errorf("%w: the cap input of %s at %s credits %d s", ErrCapMismatch, c.DeviceID, at, c.Credited)
		case !minutes[k.minute]:
			return fmt.Errorf("%w: a cap input at %s, a minute with no frozen row", ErrCapMismatch, at)
		case c.Project == project:
			if got, ok := frozen[k.dm]; !ok || got != c.Credited {
				return fmt.Errorf("%w: the cap input of %s at %s isn't a frozen row of the project", ErrCapMismatch, c.DeviceID, at)
			}
		}
	}
	for _, r := range rows {
		if got, ok := in[dmp{dm{r.DeviceID, r.Minute.Unix()}, project}]; !ok || got != r.Credited {
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
