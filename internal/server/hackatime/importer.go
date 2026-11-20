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
	Now       func() time.Time
}

// Importer copies a user's Hackatime heartbeat history into Logbook.
type Importer struct {
	st     *store.Store
	box    *secretbox.Box
	client *Client
	sink   HeartbeatSink
	log    *slog.Logger
	delay  time.Duration
	budget time.Duration
	chunk  int
	now    func() time.Time
}

// NewImporter builds an Importer.
func NewImporter(st *store.Store, box *secretbox.Box, client *Client, sink HeartbeatSink, opts ImportOptions) *Importer {
	im := &Importer{
		st: st, box: box, client: client, sink: sink, log: opts.Logger,
		delay: opts.DayDelay, budget: opts.RunBudget, chunk: opts.ChunkSize, now: opts.Now,
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
	if im.now == nil {
		im.now = time.Now
	}
	return im
}

// RunResult says whether an import run finished.
type RunResult struct {
	// Done is true when there is nothing left to import (finished, failed
	// permanently, unlinked or never started).
	Done bool
}

// errPermanent marks failures that retrying won't fix.
var errPermanent = errors.New("permanent import failure")

// Run imports days from the user's saved cursor until the import finishes or
// the run budget is spent. Progress is saved after every day, so a crash or
// cancellation resumes at the first unfinished day. lastAttempt marks the
// final retry: a failure then marks the import failed instead of pending.
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
	token, err := im.box.Open(link.AccessTokenEnc, userID[:])
	if err != nil {
		return im.fail(ctx, userID, true, fmt.Errorf("%w: decrypting token: %w", errPermanent, err))
	}
	if imp.Status != importRunning {
		if err := im.setStatus(ctx, userID, importRunning, imp.LastError); err != nil {
			return RunResult{}, err
		}
	}

	started := time.Now()
	day, last := imp.NextDate.Time, imp.ToDate.Time
	for !day.After(last) {
		if time.Since(started) > im.budget {
			return RunResult{}, nil
		}
		n, err := im.importDay(ctx, userID, string(token), day)
		if err != nil {
			return im.fail(ctx, userID, lastAttempt, err)
		}
		day = day.AddDate(0, 0, 1)
		rows, err := im.st.AdvanceHackatimeImport(ctx, db.AdvanceHackatimeImportParams{
			UserID: userID, NextDate: pgDate(day), HeartbeatsImported: int64(n),
		})
		if err != nil {
			return RunResult{}, fmt.Errorf("saving hackatime import progress: %w", err)
		}
		if rows == 0 {
			return RunResult{Done: true}, nil // unlinked mid-run
		}
		if !day.After(last) {
			if err := sleep(ctx, im.delay); err != nil {
				return RunResult{}, err
			}
		}
	}
	if err := im.setStatus(ctx, userID, importDone, ""); err != nil {
		return RunResult{}, err
	}
	im.log.InfoContext(ctx, "hackatime import finished", "user_id", userID)
	return RunResult{Done: true}, nil
}

// importDay fetches and stores one UTC day, returning the number inserted.
func (im *Importer) importDay(ctx context.Context, userID uuid.UUID, token string, day time.Time) (int, error) {
	hbs, err := im.fetchWindow(ctx, token, day, day.Add(24*time.Hour))
	if err != nil {
		return 0, err
	}
	inserted := 0
	for start := 0; start < len(hbs); start += im.chunk {
		end := min(start+im.chunk, len(hbs))
		n, err := im.sink.Ingest(ctx, userID, hbs[start:end], ImportSource)
		if err != nil {
			return 0, fmt.Errorf("storing heartbeats for %s: %w", day.Format(time.DateOnly), err)
		}
		inserted += n
	}
	return inserted, nil
}

// fetchWindow returns valid heartbeats in [from, to), halving the window when
// Hackatime's response is too large.
func (im *Importer) fetchWindow(ctx context.Context, token string, from, to time.Time) ([]wire.Heartbeat, error) {
	// Hackatime's range is inclusive at both ends; stop just short of `to`
	// so adjacent windows don't overlap.
	remote, err := im.client.Heartbeats(ctx, token, from, to.Add(-time.Microsecond))
	if errors.Is(err, ErrResponseTooLarge) && to.Sub(from) > minWindow {
		mid := from.Add(to.Sub(from) / 2)
		a, err := im.fetchWindow(ctx, token, from, mid)
		if err != nil {
			return nil, err
		}
		b, err := im.fetchWindow(ctx, token, mid, to)
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	}
	if err != nil {
		return nil, err
	}
	now := im.now()
	out := make([]wire.Heartbeat, 0, len(remote))
	for _, r := range remote {
		if h, ok := toWire(r, from, to, now); ok {
			out = append(out, h)
		}
	}
	if skipped := len(remote) - len(out); skipped > 0 {
		im.log.DebugContext(ctx, "skipped hackatime heartbeats", "count", skipped, "from", from)
	}
	return out, nil
}

// fail records err on the import and returns it. Permanent errors and the
// last attempt mark the import failed; otherwise it stays pending for retry.
func (im *Importer) fail(ctx context.Context, userID uuid.UUID, final bool, err error) (RunResult, error) {
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
	if serr := im.setStatus(ctx, userID, status, msg); serr != nil {
		err = errors.Join(err, serr)
	}
	im.log.WarnContext(ctx, "hackatime import step failed", "user_id", userID, "status", status, "err", err)
	return RunResult{Done: status == importFailed}, err
}

func (im *Importer) setStatus(ctx context.Context, userID uuid.UUID, status, msg string) error {
	// Use a fresh context so a cancelled job still records why it stopped.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := im.st.SetHackatimeImportStatus(ctx, db.SetHackatimeImportStatusParams{
		UserID: userID, Status: status, LastError: msg,
	}); err != nil {
		return fmt.Errorf("saving hackatime import status: %w", err)
	}
	return nil
}

// userMessage turns an import error into text safe to show the user. It never
// includes raw error strings, which could carry upstream response details.
func userMessage(err error) string {
	var rl *RateLimitedError
	var se *StatusError
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "Hackatime no longer accepts Logbook's access. Unlink and link your account again."
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
