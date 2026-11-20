package crediting

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// CutoffAt returns the arrival cutoff final minute m was computed with: the
// range of ranges holding m, else m + FinalLead (ADR 0016). A zero cutoff
// means the agent never computed m: no events count for it.
func CutoffAt(ranges []localdb.CutoffRange, m time.Time) time.Time {
	for _, r := range ranges {
		if !m.Before(r.From) && m.Before(r.To) {
			return r.Cutoff
		}
	}
	return m.Add(FinalLead)
}

// ReplayMinutes recomputes the final minutes [from, to) from raw events
// exactly as the agent made them final: each minute with the run its
// cutoff names (the window ending at the cutoff, over the events that had
// arrived by then, with the repo mapping journal), so events that arrived
// later never count. It returns the rows the agent stores for them, sorted
// by minute and project.
//
// Claude Code active time is not replayed: it feeds only the advisory
// otel_mismatch flag. Compare label, project, credit and cuts.
//
// compute is the engine (struggle.Compute when nil).
func ReplayMinutes(ctx context.Context, events EventReader, journal []localdb.RepoMapping, ranges []localdb.CutoffRange,
	from, to time.Time, compute func(struggle.Input) struggle.Result, tz *time.Location) ([]localdb.CreditedMinute, error) {
	if compute == nil {
		compute = struggle.Compute
	}
	if tz == nil {
		tz = time.UTC
	}
	from, to = from.UTC().Truncate(time.Minute), to.UTC().Truncate(time.Minute)
	// A cutoff is the end of a window holding its minute, so it is at most
	// Window after it, and that run read History before its window.
	ev, err := events.EventsBetween(ctx, from.Add(-History-Window), to.Add(Window), nil)
	if err != nil {
		return nil, fmt.Errorf("reading events to replay: %w", err)
	}
	byCutoff := map[time.Time][]time.Time{}
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		if c := CutoffAt(ranges, m); !c.IsZero() {
			byCutoff[c] = append(byCutoff[c], m)
		}
	}
	cutoffs := make([]time.Time, 0, len(byCutoff))
	for c := range byCutoff {
		cutoffs = append(cutoffs, c)
	}
	slices.SortFunc(cutoffs, time.Time.Compare)
	var out []localdb.CreditedMinute
	for _, c := range cutoffs {
		s := spanAt(c)
		if !s.cutoff().Equal(c) {
			return nil, fmt.Errorf("cutoff %s is not a whole minute", c.Format(time.RFC3339Nano))
		}
		mine := byCutoff[c]
		for _, row := range minuteRows(s, computeSpan(compute, s, ev, nil, journal, tz)) {
			if _, ok := slices.BinarySearchFunc(mine, row.Minute, time.Time.Compare); ok {
				out = append(out, row)
			}
		}
	}
	slices.SortFunc(out, func(a, b localdb.CreditedMinute) int {
		if c := a.Minute.Compare(b.Minute); c != 0 {
			return c
		}
		return cmp.Compare(a.Project, b.Project)
	})
	return out, nil
}

// ReplayDayCredit recomputes a sealed day's minutes with the cutoffs stored
// with it (ReplayMinutes). It returns ErrNotReproducible for a day sealed
// before cutoffs were kept, or whose cutoffs were pruned.
func ReplayDayCredit(ctx context.Context, events EventReader, e localdb.EvidenceDay, journal []localdb.RepoMapping,
	compute func(struggle.Input) struggle.Result, tz *time.Location) ([]localdb.CreditedMinute, error) {
	if e.Cutoffs == nil {
		return nil, fmt.Errorf("%s: cutoffs not kept: %w", e.Day, ErrNotReproducible)
	}
	start, end, err := evidence.DayBounds(e.Day)
	if err != nil {
		return nil, err
	}
	return ReplayMinutes(ctx, events, journal, e.Cutoffs, start, end, compute, tz)
}
