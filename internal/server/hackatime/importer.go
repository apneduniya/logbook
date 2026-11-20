package hackatime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/wire"
)

// HeartbeatSink stores imported heartbeats, deduplicating them so re-running
// an import never creates duplicates. ingest.Service implements it.
type HeartbeatSink interface {
	Ingest(ctx context.Context, userID uuid.UUID, hbs []wire.Heartbeat, source string) (inserted int, err error)
}

// ImportSource is the heartbeat source recorded for imported history.
const ImportSource = "import"

// Import statuses (hackatime_imports.status).
const (
	importPending = "pending"
	importRunning = "running"
	importDone    = "done"
	importFailed  = "failed"
)

// minWindow is the smallest slice of a day the importer will request when a
// response is too large to accept in one go.
const minWindow = 15 * time.Minute

// defaultMaxPerDay caps the heartbeats imported for one day. A heartbeat every
// two seconds around the clock is 43,200; more than that isn't plausible
// editor activity, and Hackatime heartbeats are user-controlled.
const defaultMaxPerDay = 50_000

// truncatedNote goes in last_error once a day hits a cap, so the user knows
// the history isn't complete. It is kept for the rest of the import.
const truncatedNote = "Some days had more Hackatime heartbeats than Logbook imports per day; the excess was skipped."

// stalledAfter is how long an unfinished import may go without progress
// before the sweep queues a job for it again.
const stalledAfter = 15 * time.Minute

// importSweepLimit caps how many import jobs one sweep queues.
const importSweepLimit = 500

// ImportOptions tune the importer. Zero values get defaults.
type ImportOptions struct {
	Logger *slog.Logger
	// DayDelay is the pause between days, on top of the client's global pace. Default 1s.
	DayDelay time.Duration
	// RunBudget is how long one run works before yielding; the job is then
	// rescheduled and resumes from the saved day. Default 4m.
	RunBudget time.Duration
	// ChunkSize is the most heartbeats per Ingest call. Default and max 1000.
	ChunkSize int
	// MaxPerDay caps the heartbeats imported for one UTC day. Default 50,000.
	MaxPerDay int
	Now       func() time.Time
}

// Importer copies a user's Hackatime heartbeat history into Logbook.
type Importer struct {
	st        *store.Store
	box       *secretbox.Box
	client    *Client
	sink      HeartbeatSink
	log       *slog.Logger
	delay     time.Duration
	budget    time.Duration
	chunk     int
	maxPerDay int
	now       func() time.Time
}

// NewImporter builds an Importer.
func NewImporter(st *store.Store, box *secretbox.Box, client *Client, sink HeartbeatSink, opts ImportOptions) *Importer {
	im := &Importer{
		st: st, box: box, client: client, sink: sink, log: opts.Logger,
		delay: opts.DayDelay, budget: opts.RunBudget, chunk: opts.ChunkSize,
		maxPerDay: opts.MaxPerDay, now: opts.Now,
	}
	if im.log == nil {
		im.log = slog.Default()
	}
	if im.delay < 0 {
		im.delay = 0
	} else if im.delay == 0 {
		im.delay = time.Second
	}
	if im.budget <= 0 {
		im.budget = 4 * time.Minute
	}
	if im.chunk <= 0 || im.chunk > 1000 {
		im.chunk = 1000
	}
	if im.maxPerDay <= 0 {
		im.maxPerDay = defaultMaxPerDay
	}
	if im.now == nil {
		im.now = time.Now
	}
	return im
}

// RunResult says whether an import run finished.
type RunResult struct {
	// Done is true when there is nothing left to import (finished, failed
	// permanently, unlinked or never started). A run that returns neither
	// Done nor an error must run again: it spent its budget, or the link
	// changed under it and a fresh run has to pick up the new link.
	Done bool
}

// errPermanent marks failures that retrying won't fix.
var errPermanent = errors.New("permanent import failure")

// Run imports days from the user's saved cursor until the import finishes or
// the run budget is spent. Progress is saved after every day, so a crash or
// cancellation resumes at the first unfinished day. lastAttempt marks the
// final retry: a failure then marks the import failed instead of pending.
//
// A run belongs to the link it started with (identified by linked_at, which
// changes on every link). Every write is conditional on that link still being
// current, so a run overtaken by an unlink or relink stops without touching
// the new import.
func (im *Importer) Run(ctx context.Context, userID uuid.UUID, lastAttempt bool) (RunResult, error) {
	imp, err := im.st.GetHackatimeImport(ctx, userID)
	if store.IsNotFound(err) {
		return RunResult{Done: true}, nil
	}
	if err != nil {
		return RunResult{}, fmt.Errorf("reading hackatime import: %w", err)
	}
	if imp.Status == importDone || imp.Status == importFailed {
		return RunResult{Done: true}, nil
	}
	link, err := im.st.GetHackatimeLink(ctx, userID)
	if store.IsNotFound(err) {
		return RunResult{Done: true}, nil // unlinked since the job was queued
	}
	if err != nil {
		return RunResult{}, fmt.Errorf("reading hackatime link: %w", err)
	}
	gen := link.LinkedAt
	token, err := im.box.Open(link.AccessTokenEnc, userID[:])
	if err != nil {
		return im.fail(ctx, userID, gen, true, fmt.Errorf("%w: decrypting token: %w", errPermanent, err))
	}
	if imp.Status != importRunning {
		applied, err := im.setStatus(ctx, userID, gen, importRunning, imp.LastError)
		if err != nil {
			return RunResult{}, err
		}
		if !applied {
			return im.superseded(ctx, userID)
		}
	}

	note := ""
	if imp.LastError == truncatedNote {
		note = truncatedNote
	}
	started := time.Now()
	day, last := imp.NextDate.Time, imp.ToDate.Time
	for !day.After(last) {
		if time.Since(started) > im.budget {
			return RunResult{}, nil
		}
		fetchedAt := im.now()
		res, err := im.importDay(ctx, userID, string(token), day)
		if err != nil {
			return im.fail(ctx, userID, gen, lastAttempt, err)
		}
		if res.truncated {
			note = truncatedNote
		}
		next := day.AddDate(0, 0, 1)
		partial := next.After(fetchedAt)
		if partial {
			// The day hadn't ended when it was fetched. Keep the cursor on
			// it so the next import (after a relink) fetches the rest;
			// ingest drops what we already have.
			next = day
		}
		toDate, err := im.st.AdvanceHackatimeImport(ctx, db.AdvanceHackatimeImportParams{
			NextDate: pgDate(next), HeartbeatsImported: int64(res.inserted), LastError: note,
			UserID: userID, LinkedAt: gen, PrevNextDate: pgDate(day),
		})
		if store.IsNotFound(err) {
			return im.superseded(ctx, userID)
		}
		if err != nil {
			return RunResult{}, fmt.Errorf("saving hackatime import progress: %w", err)
		}
		if partial {
			break // no later day can have ended either
		}
		day, last = next, toDate.Time
		if !day.After(last) {
			if err := sleep(ctx, im.delay); err != nil {
				return RunResult{}, err
			}
		}
	}
	n, err := im.st.FinishHackatimeImport(ctx, db.FinishHackatimeImportParams{
		LastError: note, UserID: userID, LinkedAt: gen, ToDate: pgDate(last),
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("finishing hackatime import: %w", err)
	}
	if n == 0 {
		return im.superseded(ctx, userID)
	}
	im.log.InfoContext(ctx, "hackatime import finished", "user_id", userID)
	return RunResult{Done: true}, nil
}

// superseded ends a run whose link was replaced, or whose import was
// extended, while it worked. It isn't done: the next run starts over from the
// current link and import row.
func (im *Importer) superseded(ctx context.Context, userID uuid.UUID) (RunResult, error) {
	im.log.InfoContext(ctx, "hackatime import run superseded by a link change", "user_id", userID)
	return RunResult{}, nil
}

// StalledUsers lists users whose import is unfinished but hasn't moved for a
// while, for example because a relink landed just as the previous job was
// finishing. Queueing a job for them is safe: import jobs are unique per user
// while active.
func (im *Importer) StalledUsers(ctx context.Context) ([]uuid.UUID, error) {
	ids, err := im.st.ListStalledHackatimeImports(ctx, db.ListStalledHackatimeImportsParams{
		UpdatedBefore: im.now().Add(-stalledAfter), Lim: importSweepLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing stalled hackatime imports: %w", err)
	}
	return ids, nil
}

// dayResult is what importing one day did.
type dayResult struct {
	inserted  int  // new heartbeats stored
	kept      int  // heartbeats handed to the sink, including duplicates
	truncated bool // the day hit a cap and some heartbeats were skipped
}

// importDay fetches and stores one UTC day.
func (im *Importer) importDay(ctx context.Context, userID uuid.UUID, token string, day time.Time) (dayResult, error) {
	var res dayResult
	if err := im.importWindow(ctx, userID, token, day, day.Add(24*time.Hour), &res); err != nil {
		return dayResult{}, err
	}
	if res.truncated {
		im.log.WarnContext(ctx, "hackatime import day truncated", "user_id", userID,
			"day", day.Format(time.DateOnly), "kept", res.kept)
	}
	return res, nil
}

// importWindow imports the heartbeats in [from, to), halving the window when
// Hackatime's response is too large. Each window is stored as soon as it is
// fetched, so memory is bounded by one response rather than one day. A window
// still too large at minWindow, and anything past the daily cap, is skipped
// and the day marked truncated.
func (im *Importer) importWindow(ctx context.Context, userID uuid.UUID, token string, from, to time.Time, res *dayResult) error {
	if res.kept >= im.maxPerDay {
		res.truncated = true
		return nil
	}
	// Hackatime's range is inclusive at both ends; stop just short of `to`
	// so adjacent windows don't overlap.
	remote, err := im.client.Heartbeats(ctx, token, from, to.Add(-time.Microsecond))
	if errors.Is(err, ErrResponseTooLarge) {
		if to.Sub(from) <= minWindow {
			res.truncated = true
			return nil
		}
		mid := from.Add(to.Sub(from) / 2)
		if err := im.importWindow(ctx, userID, token, from, mid, res); err != nil {
			return err
		}
		return im.importWindow(ctx, userID, token, mid, to, res)
	}
	if err != nil {
		return err
	}
	now := im.now()
	hbs := make([]wire.Heartbeat, 0, len(remote))
	for _, r := range remote {
		if h, ok := toWire(r, from, to, now); ok {
			hbs = append(hbs, h)
		}
	}
	if skipped := len(remote) - len(hbs); skipped > 0 {
		im.log.DebugContext(ctx, "skipped hackatime heartbeats", "count", skipped, "from", from)
	}
	if room := im.maxPerDay - res.kept; len(hbs) > room {
		hbs = hbs[:room]
		res.truncated = true
	}
	res.kept += len(hbs)
	for start := 0; start < len(hbs); start += im.chunk {
		end := min(start+im.chunk, len(hbs))
		n, err := im.sink.Ingest(ctx, userID, hbs[start:end], ImportSource)
		if err != nil {
			return fmt.Errorf("storing heartbeats for %s: %w", from.Format(time.DateOnly), err)
		}
		res.inserted += n
	}
	return nil
}

// fail records err on the import and returns it. Permanent errors and the
// last attempt mark the import failed; otherwise it stays pending for retry.
// If the link changed since the run started, err belongs to the old link (its
// token may have been revoked by the unlink), so nothing is recorded and the
// run reports itself superseded.
func (im *Importer) fail(ctx context.Context, userID uuid.UUID, gen time.Time, final bool, err error) (RunResult, error) {
	var rl *RateLimitedError
	permanent := errors.Is(err, errPermanent) || errors.Is(err, ErrUnauthorized)
	status := importPending
	if (final || permanent) && !errors.As(err, &rl) {
		status = importFailed
	}
	msg := userMessage(err)
	if status == importFailed && !permanent {
		msg = "The import stopped after repeated errors. Link your Hackatime account again to resume."
	}
	applied, serr := im.setStatus(ctx, userID, gen, status, msg)
	switch {
	case serr != nil:
		err = errors.Join(err, serr)
	case !applied:
		return im.superseded(ctx, userID)
	}
	im.log.WarnContext(ctx, "hackatime import step failed", "user_id", userID, "status", status, "err", err)
	return RunResult{Done: status == importFailed}, err
}

// setStatus sets the import's status if link generation gen is still
// current, and reports whether it did.
func (im *Importer) setStatus(ctx context.Context, userID uuid.UUID, gen time.Time, status, msg string) (bool, error) {
	// Use a fresh context so a cancelled job still records why it stopped.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	n, err := im.st.SetHackatimeImportStatus(ctx, db.SetHackatimeImportStatusParams{
		Status: status, LastError: msg, UserID: userID, LinkedAt: gen,
	})
	if err != nil {
		return false, fmt.Errorf("saving hackatime import status: %w", err)
	}
	return n > 0, nil
}

// userMessage turns an import error into text safe to show the user. It never
// includes raw error strings, which could carry upstream response details.
func userMessage(err error) string {
	var rl *RateLimitedError
	var se *StatusError
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "Hackatime no longer accepts Logbook's access. Link your Hackatime account again."
	case errors.Is(err, errPermanent):
		return "The saved Hackatime access can't be read. Unlink and link your account again."
	case errors.As(err, &rl):
		return "Hackatime is rate limiting imports. It will continue automatically."
	case errors.Is(err, ErrInvalidResponse), errors.Is(err, ErrResponseTooLarge):
		return "Hackatime returned data Logbook couldn't read. It will retry."
	case errors.As(err, &se):
		return fmt.Sprintf("Hackatime returned an error (HTTP %d). It will retry.", se.Code)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "The import was interrupted. It will continue automatically."
	default:
		return "The import hit an error. It will retry."
	}
}
