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

// probeDays is how many days one spans request covers. A month of spans is
// tens of KiB even for a busy user, and cheap for Hackatime to compute.
const probeDays = 31

// maxPlanDays bounds how far ahead the importer probes before fetching, so
// the saved cursor (and the progress the user sees) moves at least every
// six probes even through a long quiet stretch.
const maxPlanDays = 6 * probeDays

// defaultWindowActivity is how much Hackatime activity one heartbeats request
// should cover. Editors send a heartbeat every few tens of seconds while
// active, so eight hours is around a thousand heartbeats (about 1 MiB), well
// under the response cap; a denser window is split as before.
const defaultWindowActivity = 8 * time.Hour

// defaultMaxPerDay caps the heartbeats imported for one day. A heartbeat every
// two seconds around the clock is 43,200; more than that isn't plausible
// editor activity, and Hackatime heartbeats are user-controlled.
const defaultMaxPerDay = 50_000

// truncatedNote goes in last_error once a day hits a cap, so the user knows
// the history isn't complete. It is kept for the rest of the import.
const truncatedNote = "Some days had more Hackatime heartbeats than Logbook imports per day; the excess was skipped."

// recentDays is how many of the latest days (up to and including the day of
// linking) the import fetches before backfilling older history, so the
// dashboard fills in within seconds of linking.
const recentDays = 30

// defaultConcurrency is how many windows one import fetches at a time. Every
// request still waits on the client's process-wide limiter, so this only
// overlaps one window's ingest with the next window's fetch; it never raises
// the request rate.
const defaultConcurrency = 2

// maxConcurrency bounds ImportOptions.Concurrency. Each window in flight
// holds at most one response in memory.
const maxConcurrency = 8

// stalledAfter is how long an unfinished import may go without progress
// before the sweep queues a job for it again.
const stalledAfter = 15 * time.Minute

// importSweepLimit caps how many import jobs one sweep queues.
const importSweepLimit = 500

// ImportOptions tune the importer. Zero values get defaults.
type ImportOptions struct {
	Logger *slog.Logger
	// WindowDelay is the pause after each heartbeats window, on top of the
	// client's global pace. Spans probes are cheap and only paced. Default
	// 500ms; negative disables it.
	WindowDelay time.Duration
	// WindowActivity is how much Hackatime activity (from the spans probe)
	// one heartbeats request should cover. Default 8h.
	WindowActivity time.Duration
	// RunBudget is how long one run works before yielding; the job is then
	// rescheduled and resumes from the saved day. Default 4m.
	RunBudget time.Duration
	// ChunkSize is the most heartbeats per Ingest call. Default and max 1000.
	ChunkSize int
	// MaxPerDay caps the heartbeats imported for one UTC day. Default 50,000.
	MaxPerDay int
	// Concurrency is how many windows one import fetches at a time. Default
	// 2, at most 8. All imports and the trust refresher share the client's
	// limiter, so this bounds one import's share of the request budget
	// rather than adding to it.
	Concurrency int
	Now         func() time.Time
}

// Importer copies a user's Hackatime heartbeat history into Logbook.
type Importer struct {
	st             *store.Store
	box            *secretbox.Box
	client         *Client
	sink           HeartbeatSink
	log            *slog.Logger
	delay          time.Duration
	windowActivity time.Duration
	budget         time.Duration
	chunk          int
	maxPerDay      int
	concurrency    int
	now            func() time.Time
}

// NewImporter builds an Importer.
func NewImporter(st *store.Store, box *secretbox.Box, client *Client, sink HeartbeatSink, opts ImportOptions) *Importer {
	im := &Importer{
		st: st, box: box, client: client, sink: sink, log: opts.Logger,
		delay: opts.WindowDelay, windowActivity: opts.WindowActivity, budget: opts.RunBudget,
		chunk: opts.ChunkSize, maxPerDay: opts.MaxPerDay, concurrency: opts.Concurrency, now: opts.Now,
	}
	if im.log == nil {
		im.log = slog.Default()
	}
	if im.delay < 0 {
		im.delay = 0
	} else if im.delay == 0 {
		im.delay = 500 * time.Millisecond
	}
	if im.windowActivity <= 0 {
		im.windowActivity = defaultWindowActivity
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
	if im.concurrency <= 0 {
		im.concurrency = defaultConcurrency
	}
	im.concurrency = min(im.concurrency, maxConcurrency)
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

// Run imports the user's history until the import finishes or the run budget
// is spent, in two phases. The recent phase fetches the last recentDays days
// first, so the dashboard fills in within seconds of linking; the backfill
// phase then fetches everything older, oldest first. Each phase asks
// Hackatime where the user was active (a spans probe per month), then fetches
// the heartbeats in windows of whole UTC days sized to that activity, up to
// the configured number of windows at a time. Every day in the range is still
// fetched: the probe only sizes windows. Progress is saved as windows finish,
// and a phase's cursor only moves past windows that finished with every
// earlier window, so a crash or cancellation resumes at the first unfinished
// window. lastAttempt marks the final retry: a failure then marks the import
// failed instead of pending.
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

	r := &importRun{im: im, userID: userID, gen: gen, token: string(token), started: time.Now()}
	if imp.LastError == truncatedNote {
		r.note = truncatedNote
	}
	last := utcDay(imp.ToDate.Time)
	if !imp.RecentDone {
		end, err := r.phase(ctx, recentPhase, utcDay(imp.RecentNext.Time), last.AddDate(0, 0, 1))
		if end != phaseComplete || err != nil {
			return r.stop(ctx, end, err, lastAttempt)
		}
	}
	// While the backfill has caught up, the recent phase moved next_date
	// along with it, and there is nothing older left to fetch.
	if next, recentFrom := utcDay(imp.NextDate.Time), utcDay(imp.RecentFrom.Time); next.Before(recentFrom) {
		end, err := r.phase(ctx, backfillPhase, next, recentFrom)
		if end != phaseComplete || err != nil {
			return r.stop(ctx, end, err, lastAttempt)
		}
	}
	n, err := im.st.FinishHackatimeImport(ctx, db.FinishHackatimeImportParams{
		LastError: r.note, UserID: userID, LinkedAt: gen, ToDate: pgDate(last),
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

// window is a run of whole UTC days, [from, to), fetched as one unit.
type window struct{ from, to time.Time }

// plan probes Hackatime's activity spans a month at a time from `from` and
// returns windows that tile [from, end) for some end no later than the day
// after last. It keeps probing through quiet months, so a long empty stretch
// becomes part of a single window, until it has seen a window's worth of
// activity or maxPlanDays.
//
// If a probe fails for any reason other than cancellation or a rate limit,
// the month it covered is planned one day per window, as the importer
// worked before it probed. A probe never causes a day to be skipped.
func (im *Importer) plan(ctx context.Context, token string, from, last time.Time) ([]window, error) {
	end := last.AddDate(0, 0, 1)
	activity := map[time.Time]time.Duration{}
	var total time.Duration
	for chunkFrom := from; ; {
		chunkTo := earliest(chunkFrom.AddDate(0, 0, probeDays), end)
		spans, err := im.client.Spans(ctx, token, chunkFrom, chunkTo.Add(-time.Microsecond))
		if err != nil {
			var rl *RateLimitedError
			if ctx.Err() != nil || errors.As(err, &rl) {
				return nil, err
			}
			im.log.WarnContext(ctx, "hackatime activity probe failed; importing day by day",
				"from", chunkFrom.Format(time.DateOnly), "err", err)
			var windows []window
			if chunkFrom.After(from) {
				windows = planWindows(from, chunkFrom, activity, im.windowActivity)
			}
			return append(windows, dailyWindows(chunkFrom, chunkTo)...), nil
		}
		for _, s := range spans {
			if d := utcDay(s.Start); !d.Before(chunkFrom) && d.Before(chunkTo) {
				activity[d] += s.Duration
				total += s.Duration
			}
		}
		chunkFrom = chunkTo
		if !chunkFrom.Before(end) || total >= im.windowActivity || chunkFrom.Sub(from) >= maxPlanDays*24*time.Hour {
			return planWindows(from, chunkFrom, activity, im.windowActivity), nil
		}
	}
}

// planWindows tiles the days [from, to) with windows. Each window covers
// consecutive days and closes before a day that would take its activity past
// budget, so idle days are absorbed into their neighbours and a busy day gets
// a window of its own. activity is keyed by UTC day.
func planWindows(from, to time.Time, activity map[time.Time]time.Duration, budget time.Duration) []window {
	var windows []window
	start, sum := from, time.Duration(0)
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		a := activity[d]
		if sum > 0 && sum+a > budget {
			windows = append(windows, window{start, d})
			start, sum = d, 0
		}
		sum += a
	}
	return append(windows, window{start, to})
}

// recentStart is the first day of the recent phase for an import of
// [from, to]: the last recentDays days, or all of them if there are fewer.
func recentStart(from, to time.Time) time.Time {
	return latest(utcDay(from), utcDay(to).AddDate(0, 0, 1-recentDays))
}

// utcDay is the start of t's UTC day.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// dailyWindows is one window per day in [from, to).
func dailyWindows(from, to time.Time) []window {
	var windows []window
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		windows = append(windows, window{d, d.AddDate(0, 0, 1)})
	}
	return windows
}

// windowResult is what importing one window did.
type windowResult struct {
	inserted  int                // new heartbeats stored
	kept      map[time.Time]int  // heartbeats handed to the sink per UTC day, including duplicates
	truncated map[time.Time]bool // days that hit a cap and had heartbeats skipped
}

// importWindow fetches and stores one window.
func (im *Importer) importWindow(ctx context.Context, userID uuid.UUID, token string, w window) (windowResult, error) {
	res := windowResult{kept: map[time.Time]int{}, truncated: map[time.Time]bool{}}
	if err := im.importRange(ctx, userID, token, w.from, w.to, &res); err != nil {
		return windowResult{}, err
	}
	for d := range res.truncated {
		im.log.WarnContext(ctx, "hackatime import day truncated", "user_id", userID,
			"day", d.Format(time.DateOnly), "kept", res.kept[d])
	}
	return res, nil
}

// capped reports whether every UTC day overlapping [from, to) has reached
// the daily cap, so there is nothing left to fetch there.
func (im *Importer) capped(res *windowResult, from, to time.Time) bool {
	for d := utcDay(from); d.Before(to); d = d.AddDate(0, 0, 1) {
		if res.kept[d] < im.maxPerDay {
			return false
		}
	}
	return true
}

// importRange imports the heartbeats in [from, to), halving the range when
// Hackatime's response is too large. Each range is stored as soon as it is
// fetched, so memory is bounded by one response rather than one window. A
// range still too large at minWindow, and anything past a day's cap, is
// skipped and the day marked truncated.
func (im *Importer) importRange(ctx context.Context, userID uuid.UUID, token string, from, to time.Time, res *windowResult) error {
	if im.capped(res, from, to) {
		res.truncated[utcDay(from)] = true
		return nil
	}
	// Hackatime's range is inclusive at both ends; stop just short of `to`
	// so adjacent windows don't overlap.
	remote, err := im.client.Heartbeats(ctx, token, from, to.Add(-time.Microsecond))
	if errors.Is(err, ErrResponseTooLarge) {
		if to.Sub(from) <= minWindow {
			res.truncated[utcDay(from)] = true
			return nil
		}
		mid := from.Add(to.Sub(from) / 2)
		if err := im.importRange(ctx, userID, token, from, mid, res); err != nil {
			return err
		}
		return im.importRange(ctx, userID, token, mid, to, res)
	}
	if err != nil {
		return err
	}
	now := im.now()
	hbs := make([]wire.Heartbeat, 0, len(remote))
	invalid := 0
	for _, r := range remote {
		h, ok := toWire(r, from, to, now)
		if !ok {
			invalid++
			continue
		}
		// Hackatime returns heartbeats oldest first, so each day keeps
		// its earliest heartbeats up to the cap.
		day := utcDay(h.Timestamp())
		if res.kept[day] >= im.maxPerDay {
			res.truncated[day] = true
			continue
		}
		res.kept[day]++
		hbs = append(hbs, h)
	}
	if invalid > 0 {
		im.log.DebugContext(ctx, "skipped hackatime heartbeats", "count", invalid, "from", from)
	}
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
