package hackatime

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// importPhase is which cursor a phase of the import moves.
type importPhase int

const (
	// recentPhase fills [recent_next, to_date], the latest recentDays days.
	recentPhase importPhase = iota
	// backfillPhase fills [next_date, recent_from), oldest first.
	backfillPhase
)

// phaseEnd is how a phase, or a batch of windows inside it, stopped.
type phaseEnd int

const (
	phaseContinue    phaseEnd = iota // every window saved; more may follow
	phaseComplete                    // nothing left in the phase
	phaseYield                       // run budget spent; the next run resumes
	phaseSuperseded                  // the link changed under the run
	phaseFetchFailed                 // Hackatime or ingest failed; recorded on the import
	phaseStoreFailed                 // saving progress failed
)

// importRun is one Run's state, shared by both phases.
type importRun struct {
	im      *Importer
	userID  uuid.UUID
	gen     time.Time // linked_at of the link the run belongs to
	token   string
	started time.Time
	note    string // last_error to save: "" or truncatedNote
	windows int    // windows started in this run
}

// stop turns a phase that ended early into Run's result.
func (r *importRun) stop(ctx context.Context, end phaseEnd, err error, lastAttempt bool) (RunResult, error) {
	switch end {
	case phaseYield:
		return RunResult{}, nil
	case phaseSuperseded:
		return r.im.superseded(ctx, r.userID)
	case phaseFetchFailed:
		return r.im.fail(ctx, r.userID, r.gen, lastAttempt, err)
	case phaseStoreFailed:
		if ctx.Err() != nil {
			// Cancelled mid-save: record it as interrupted, as a cancelled fetch is.
			return r.im.fail(ctx, r.userID, r.gen, lastAttempt, err)
		}
		return RunResult{}, err
	default:
		return RunResult{}, err
	}
}

// overBudget reports whether the run should yield. A run always gets to
// start at least one window, so every run makes progress.
func (r *importRun) overBudget() bool {
	return r.windows > 0 && time.Since(r.started) > r.im.budget
}

// phase imports the days [from, end) for phase p: it plans windows from the
// activity probe and runs them, until the phase is done or stops early. When
// the recent phase is done it is marked so, and later runs skip it.
func (r *importRun) phase(ctx context.Context, p importPhase, from, end time.Time) (phaseEnd, error) {
	cursor := from
	for cursor.Before(end) {
		if r.overBudget() {
			return phaseYield, nil
		}
		windows, err := r.im.plan(ctx, r.token, cursor, end.AddDate(0, 0, -1))
		if err != nil {
			return phaseFetchFailed, err
		}
		res, next, err := r.runWindows(ctx, p, windows)
		if res != phaseContinue && res != phaseComplete {
			return res, err
		}
		cursor = next
		if res == phaseComplete {
			break // stopped on a day that hadn't ended yet
		}
	}
	if p == recentPhase {
		applied, err := r.save(ctx, p, cursor, cursor, 0, true)
		if err != nil {
			return phaseStoreFailed, err
		}
		if !applied {
			return phaseSuperseded, nil
		}
	}
	return phaseComplete, nil
}

// windowOutcome is what fetching one window did.
type windowOutcome struct {
	i         int // index into the batch
	res       windowResult
	fetchedAt time.Time
	err       error
}

// runWindows imports a batch of consecutive windows, up to im.concurrency at
// a time, starting them oldest first. Windows can finish in any order, so
// each finished window saves the heartbeats it stored straight away but moves
// the phase's cursor only over the windows that have finished with all the
// windows before them. A gap (a window still in flight, or one that failed)
// holds the cursor back, and the next run fetches from there again; ingest
// drops the duplicates.
//
// After the first failure, the budget running out or the link changing, no
// new window starts; windows already in flight finish (and are saved, unless
// the link changed) before it returns. It returns how the batch stopped and
// the saved cursor.
func (r *importRun) runWindows(ctx context.Context, p importPhase, windows []window) (phaseEnd, time.Time, error) {
	results := make(chan windowOutcome)
	finished := make([]*windowOutcome, len(windows))
	cursor := windows[0].from
	started, saved, inFlight := 0, 0, 0
	end, endErr := phaseContinue, error(nil)
	halt := func(e phaseEnd, err error) {
		if end == phaseContinue || end == phaseYield {
			end, endErr = e, err
		}
	}
	for {
		for end == phaseContinue && inFlight < r.im.concurrency && started < len(windows) {
			if r.overBudget() {
				end = phaseYield
				break
			}
			var delay time.Duration
			if r.windows > 0 {
				delay = r.im.delay
			}
			go r.fetch(ctx, started, windows[started], delay, results)
			started++
			inFlight++
			r.windows++
		}
		if inFlight == 0 {
			break
		}
		o := <-results
		inFlight--
		if o.err != nil {
			halt(phaseFetchFailed, o.err)
			continue
		}
		if len(o.res.truncated) > 0 {
			r.note = truncatedNote
		}
		if end == phaseSuperseded || end == phaseStoreFailed {
			continue // the import is no longer ours to write
		}
		finished[o.i] = &o
		next, partial := cursor, false
		for !partial && saved < len(windows) && finished[saved] != nil {
			w, f := windows[saved], finished[saved]
			next = w.to
			if w.to.After(f.fetchedAt) {
				// The last day hadn't ended when it was fetched. Keep the
				// cursor on it so the next import (after a relink) fetches
				// the rest; ingest drops what we already have. No later
				// day can have ended either, so this is the last window.
				next, partial = latest(w.from, utcDay(f.fetchedAt)), true
			}
			saved++
		}
		applied, err := r.save(ctx, p, cursor, next, o.res.inserted, false)
		switch {
		case err != nil:
			halt(phaseStoreFailed, err)
			continue
		case !applied:
			halt(phaseSuperseded, nil)
			continue
		}
		cursor = next
		if partial {
			halt(phaseComplete, nil)
		}
	}
	return end, cursor, endErr
}

// fetch imports one window, after pausing for delay, and reports the outcome.
func (r *importRun) fetch(ctx context.Context, i int, w window, delay time.Duration, out chan<- windowOutcome) {
	o := windowOutcome{i: i}
	if o.err = sleep(ctx, delay); o.err == nil {
		o.fetchedAt = r.im.now()
		o.res, o.err = r.im.importWindow(ctx, r.userID, r.token, w)
	}
	out <- o
}

// save records inserted heartbeats and moves phase p's cursor from prev to
// next (which may equal prev). recentDone marks the recent phase finished.
// It reports false when the link changed or another writer moved the cursor.
func (r *importRun) save(ctx context.Context, p importPhase, prev, next time.Time, inserted int, recentDone bool) (bool, error) {
	var n int64
	var err error
	if p == recentPhase {
		n, err = r.im.st.AdvanceHackatimeRecent(ctx, db.AdvanceHackatimeRecentParams{
			RecentNext: pgDate(next), RecentDone: recentDone, HeartbeatsImported: int64(inserted),
			LastError: r.note, UserID: r.userID, LinkedAt: r.gen, PrevRecentNext: pgDate(prev),
		})
	} else {
		n, err = r.im.st.AdvanceHackatimeBackfill(ctx, db.AdvanceHackatimeBackfillParams{
			NextDate: pgDate(next), HeartbeatsImported: int64(inserted),
			LastError: r.note, UserID: r.userID, LinkedAt: r.gen, PrevNextDate: pgDate(prev),
		})
	}
	if err != nil {
		return false, fmt.Errorf("saving hackatime import progress: %w", err)
	}
	return n > 0, nil
}
