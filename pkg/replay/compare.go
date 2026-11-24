package replay

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// ErrUnknownRules means a rules version this build can't run.
var ErrUnknownRules = errors.New("unknown rules version")

// Engine returns the engine for a rules version. A minute is always
// replayed with the version that produced it.
func Engine(rules string) (func(struggle.Input) struggle.Result, error) {
	if rules != struggle.RulesVersion {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRules, rules)
	}
	return struggle.Compute, nil
}

// Run replays key over rows with the engine of a rules version: the result
// of the engine over Input(rows, key, tz).
func Run(rows []evidence.Row, key Key, rules string, tz *time.Location) (struggle.Result, error) {
	compute, err := Engine(rules)
	if err != nil {
		return struggle.Result{}, err
	}
	in, err := Input(rows, key, tz)
	if err != nil {
		return struggle.Result{}, err
	}
	return compute(in), nil
}

// Credit is what a replay compares in a credited minute row (ADR 0017):
// project, label, credit and cuts. Flags are left out: the time zone and
// Claude Code active time feed only flags, and a replay has neither the
// same way.
type Credit struct {
	Project  string
	Label    string
	Credited int
	Cuts     []string
}

// Credits returns the engine's rows for minute m (UTC, truncated to the
// minute) as Credits.
func Credits(res struggle.Result, m time.Time) []Credit {
	m = m.UTC().Truncate(time.Minute)
	var out []Credit
	for _, r := range res.Minutes {
		if r.Minute.UTC().Truncate(time.Minute).Equal(m) {
			out = append(out, Credit{Project: r.Project, Label: string(r.Label), Credited: r.Credited, Cuts: r.Cuts})
		}
	}
	return out
}

// Same reports whether two sets of rows of one minute agree on what a
// replay compares: the same projects, each with the same label, credit and
// cuts, in any order. A row that says only that nothing happened (idle, no
// project, no credit) counts as no row, since the agent doesn't keep one
// unless it carries flags, and flags aren't compared. Removal rows (ADR
// 0014) aren't minute rows; leave them out of both sets.
func Same(a, b []Credit) bool {
	x, y := normal(a), normal(b)
	return slices.EqualFunc(x, y, func(p, q Credit) bool {
		return p.Project == q.Project && p.Label == q.Label && p.Credited == q.Credited && slices.Equal(p.Cuts, q.Cuts)
	})
}

// normal drops the rows that say nothing and sorts the rest, and their
// cuts, without changing the caller's slices.
func normal(rows []Credit) []Credit {
	out := make([]Credit, 0, len(rows))
	for _, r := range rows {
		if r.Label == string(struggle.LabelIdle) && r.Project == "" && r.Credited == 0 {
			continue
		}
		r.Cuts = slices.Sorted(slices.Values(r.Cuts))
		out = append(out, r)
	}
	slices.SortFunc(out, func(p, q Credit) int {
		return cmp.Or(cmp.Compare(p.Project, q.Project), cmp.Compare(p.Label, q.Label), cmp.Compare(p.Credited, q.Credited))
	})
	return out
}
