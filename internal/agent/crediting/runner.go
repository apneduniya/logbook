package crediting

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/upload"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// Defaults for a Runner.
const (
	DefaultInterval = time.Minute
	// runOffset delays each run a few seconds past the minute so the last
	// activity sample of the minute that just ended is stored.
	runOffset = 5 * time.Second
	// runTimeout bounds one run, uploads included.
	runTimeout  = 2 * time.Minute
	pruneEvery  = 6 * time.Hour
	maxRetryAft = 30 * time.Minute
	// maxBatchesPerRun bounds one run's minute uploads (a long offline
	// backlog drains over several runs).
	maxBatchesPerRun = 20
	// maxDaysPerRun bounds how many evidence days one run seals.
	maxDaysPerRun = 7
)

// Store is the local storage the Runner needs. *localdb.DB satisfies it.
type Store interface {
	EventsBetween(ctx context.Context, from, to time.Time, upto *localdb.HighWater) (localdb.Events, error)
	EventsAfter(ctx context.Context, after, upto localdb.HighWater, from, to time.Time) (localdb.Events, error)
	HighWater(ctx context.Context) (localdb.HighWater, error)
	SaveMinutes(ctx context.Context, u localdb.MinuteUpdate) (localdb.SaveResult, error)
	UnsentMinutes(ctx context.Context, finalBefore time.Time, limit int) ([]localdb.CreditedMinute, error)
	MarkMinutesSent(ctx context.Context, ms []localdb.CreditedMinute, finalBefore time.Time) error
	RetireMinutesBefore(ctx context.Context, t time.Time) error
	UnsentMinuteCount(ctx context.Context) (int64, error)
	UnsentMinutesBefore(ctx context.Context, t time.Time) (int64, error)
	RecordRepos(ctx context.Context, at time.Time, repos []struggle.Repo) error
	RepoMap(ctx context.Context) ([]localdb.RepoMapping, error)
	PruneCrediting(ctx context.Context) (localdb.CreditPruneResult, error)
	InsertEvidenceDay(ctx context.Context, e localdb.EvidenceDay) error
	LastEvidenceDay(ctx context.Context, deviceID string) (localdb.EvidenceDay, bool, error)
	SetEvidenceState(ctx context.Context, deviceID, day, state, errMsg string, marks ...localdb.KV) error
	Get(ctx context.Context, key string) (string, error)
	SetMany(ctx context.Context, kvs ...localdb.KV) error
	Add(ctx context.Context, key string, n int64) error
}

// Server is the server API the Runner calls. *serverapi.Client satisfies it.
type Server interface {
	UploadMinutes(ctx context.Context, t serverapi.Target, deviceID string, req serverapi.MinutesRequest) (serverapi.MinutesResult, error)
	UploadEvidenceDay(ctx context.Context, t serverapi.Target, deviceID string, req serverapi.EvidenceDayRequest) error
}

// Identity keeps the device registered. *devicekey.Identity satisfies it.
type Identity interface {
	Ensure(ctx context.Context, t serverapi.Target) (devicekey.Device, error)
	// MarkRevoked returns repaired = true when it already retired the key
	// so a new device registers next, and false when the device stays
	// revoked until the API key changes.
	MarkRevoked(ctx context.Context, t serverapi.Target) (repaired bool, err error)
	Confirm(ctx context.Context, t serverapi.Target) error
	Forget(ctx context.Context) error
}

// ActiveTimeSource returns Claude Code active-time datapoints in [from, to).
// It is optional.
type ActiveTimeSource interface {
	ActiveTimes(ctx context.Context, from, to time.Time) ([]struggle.ActiveTime, error)
}

// RepoSource lists recently active repositories. It is optional. Its answers
// are recorded in the local repo mapping journal, which is what the engine
// reads (ReposAt).
type RepoSource interface {
	ActiveRepos(ctx context.Context) ([]struggle.Repo, error)
}

// Target is where uploads go, resolved on every run.
type Target struct {
	serverapi.Target
	// Disabled, when non-empty, says why nothing is uploaded ("not signed in").
	Disabled string
}

// Options configures a Runner. Zero values take defaults.
type Options struct {
	Store    Store
	Server   Server
	Identity Identity
	Target   func() Target
	// ActiveTimes and Repos are optional; nil means none.
	ActiveTimes ActiveTimeSource
	Repos       RepoSource
	// Compute is the engine (struggle.Compute).
	Compute func(struggle.Input) struggle.Result
	// TZ is the user's time zone (time.Local).
	TZ     *time.Location
	Logger *slog.Logger
	Now    func() time.Time

	Interval, MinBackoff, MaxBackoff, Reprobe time.Duration
	// Batch is the minutes per upload (serverapi.MaxMinutes).
	Batch int
	// Jitter returns a value in [0, 1).
	Jitter func() float64
}

// Runner computes, stores and uploads credited minutes and evidence days.
type Runner struct {
	opt     Options
	running sync.Mutex // held for the length of one run
	// Used only while running is held:
	cache eventCache
	// holdMinutes is when minute uploads may resume after the server
	// refused a batch as a whole for a reason other than its size.
	holdMinutes time.Time
	// shown caches persisted status values (refused, local evidence error),
	// so they are written only when they change.
	shown map[string]string
	// clockBehind is set while the clock is before the last sealed day;
	// clockChecked once a run has found it isn't. server is the server's
	// clock from this run's latest minutes response.
	clockBehind, clockChecked bool
	server                    serverClock

	mu    sync.Mutex
	gate  gate
	batch int
}

// New returns a Runner.
func New(o Options) *Runner {
	if o.Compute == nil {
		o.Compute = struggle.Compute
	}
	if o.TZ == nil {
		o.TZ = time.Local
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = upload.DefaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = upload.DefaultMaxBackoff
	}
	if o.Reprobe <= 0 {
		o.Reprobe = upload.DefaultReprobe
	}
	if o.Batch <= 0 || o.Batch > serverapi.MaxMinutes {
		o.Batch = serverapi.MaxMinutes
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64 //nolint:gosec // jitter needs no cryptographic randomness
	}
	return &Runner{opt: o, batch: o.Batch}
}

// Run runs the pipeline every Interval (a few seconds past each minute with
// the default interval) until ctx is done. It always returns nil.
func (r *Runner) Run(ctx context.Context) error {
	var lastPrune time.Time
	timer := time.NewTimer(r.untilNext())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if now := r.opt.Now(); now.Sub(lastPrune) >= pruneEvery {
			r.prune(ctx)
			lastPrune = now
		}
		r.RunOnce(ctx)
		timer.Reset(r.untilNext())
	}
}

// untilNext returns the wait until the next run.
func (r *Runner) untilNext() time.Duration {
	if r.opt.Interval != DefaultInterval {
		return r.opt.Interval
	}
	now := r.opt.Now()
	next := now.Truncate(time.Minute).Add(runOffset)
	if !next.After(now) {
		next = next.Add(time.Minute)
	}
	return next.Sub(now)
}

// NextAttempt returns when a backing-off uploader tries again (zero if not
// backing off).
func (r *Runner) NextAttempt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate.nextTry.After(r.opt.Now()) {
		return r.gate.nextTry
	}
	return time.Time{}
}

// RunOnce computes the last hour's minutes and uploads what is due. If a
// run is already in progress it returns at once: runs never overlap.
func (r *Runner) RunOnce(ctx context.Context) {
	if !r.running.TryLock() {
		return
	}
	defer r.running.Unlock()
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	now := r.opt.Now()
	through, err := r.sealedThrough(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.ErrorContext(ctx, "reading the last sealed day", "err", err)
		}
		return
	}
	if !r.checkClock(ctx, now, through) {
		return
	}
	if err := r.compute(ctx, now, through); err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.ErrorContext(ctx, "computing credited minutes", "err", err)
		}
		return
	}
	r.upload(ctx, now)
	if n, err := r.opt.Store.UnsentMinuteCount(ctx); err == nil {
		r.persist(ctx, localdb.KV{Key: keyWaiting, Value: strconv.FormatInt(n, 10)})
	}
}

// compute runs the engine over the window and stores the result. Minutes
// before through (the end of the last sealed day) are never stored: the
// engine still reads the whole window, so the rest come out the same.
func (r *Runner) compute(ctx context.Context, now, through time.Time) error {
	s := spanAt(now)
	if err := r.cache.refresh(ctx, r.opt.Store, s.history, s.to); err != nil {
		return err
	}
	ev := r.cache.before(s.to)
	var err error
	var active []struggle.ActiveTime
	if r.opt.ActiveTimes != nil {
		if active, err = r.opt.ActiveTimes.ActiveTimes(ctx, s.history, s.to); err != nil {
			// Optional signal: credit without it rather than not at all.
			r.opt.Logger.WarnContext(ctx, "reading active time; crediting without it", "err", err)
			active = nil
		}
	}
	mapping, err := r.repoMap(ctx, now)
	if err != nil {
		return err
	}
	res := r.computeByMapping(s, ev, active, mapping)
	keep := s
	if keep.from.Before(through) {
		keep.from = through
	}
	if !keep.to.After(keep.from) {
		return nil
	}
	saved, err := r.opt.Store.SaveMinutes(ctx, localdb.MinuteUpdate{
		From: keep.from, To: keep.to, OpenFrom: s.finalBefore, Minutes: minuteRows(keep, res), Superseded: superseded,
	})
	if err != nil {
		return err
	}
	r.opt.Logger.DebugContext(ctx, "credited minutes computed", "inserted", saved.Inserted,
		"updated", saved.Updated, "deleted", saved.Deleted)
	r.persist(ctx, localdb.KV{Key: keyLastComputed, Value: formatTime(now)})
	return nil
}

// repoMap records the active repos in the mapping journal and returns the
// whole journal. If the source fails, the journal still holds the last
// known mapping.
func (r *Runner) repoMap(ctx context.Context, now time.Time) ([]localdb.RepoMapping, error) {
	if r.opt.Repos != nil {
		repos, err := r.opt.Repos.ActiveRepos(ctx)
		if err != nil {
			r.opt.Logger.WarnContext(ctx, "listing active repos; using the last known mapping", "err", err)
		} else if err := r.opt.Store.RecordRepos(ctx, now, repos); err != nil {
			return nil, err
		}
	}
	return r.opt.Store.RepoMap(ctx)
}

// computeByMapping runs the engine once per run of minutes that share a repo
// mapping (ReposAt) and joins the results: each minute comes from the run
// computed with the mapping that stood when it became final, never with one
// recorded after.
//
// Only the mapping differs between those runs. Each one is over the whole
// span, with the same events, active time and time zone, and with DataEnd at
// the span's end: the engine reads the window's bounds as well as which
// minutes to emit (a turn quiet for 10 minutes before the end of the data
// has ended; long_day and otel_mismatch total the window), so cutting the
// window per group would change a group's minutes even when the mapping
// moves no event to another project.
func (r *Runner) computeByMapping(s span, ev localdb.Events, active []struggle.ActiveTime, mapping []localdb.RepoMapping) struggle.Result {
	var out struggle.Result
	for _, g := range repoGroups(mapping, s.from, s.to) {
		in := engineInput(s, ev, active, g.repos, r.opt.TZ)
		in.DataEnd = s.to
		res := r.opt.Compute(in)
		out.RulesVersion = res.RulesVersion
		for _, m := range res.Minutes {
			if at := m.Minute.UTC().Truncate(time.Minute); !at.Before(g.from) && at.Before(g.to) {
				out.Minutes = append(out.Minutes, m)
			}
		}
	}
	return out
}

// upload registers the device if needed, then sends minutes and evidence.
func (r *Runner) upload(ctx context.Context, now time.Time) {
	t := r.opt.Target()
	if !r.ready(ctx, t, now) {
		return
	}
	r.server = serverClock{}
	dev, err := r.opt.Identity.Ensure(ctx, t.Target)
	if errors.Is(err, devicekey.ErrRevoked) {
		r.setRevoked(ctx, t.APIKey)
		return
	}
	if err != nil {
		r.fail(ctx, t, err)
		return
	}
	if dev.Replaced {
		// A new device starts clean: minutes from before it registered
		// belong to the previous device and are not sent again.
		if err := r.opt.Store.RetireMinutesBefore(ctx, dev.RegisteredAt.Truncate(time.Minute)); err != nil {
			r.opt.Logger.ErrorContext(ctx, "retiring minutes of the previous device", "err", err)
			return
		}
		// Its evidence chain starts over too.
		r.persist(ctx, localdb.KV{Key: keyEvidenceDevice, Value: dev.ID}, localdb.KV{Key: keyEvidenceDay, Value: ""},
			localdb.KV{Key: keyEvidenceHash, Value: ""}, localdb.KV{Key: keyEvidenceState, Value: ""},
			localdb.KV{Key: keyEvidenceError, Value: ""})
	}
	// Sealing a day closes it to minutes on the server, so new days are
	// sealed only on a run whose minute uploads went through; the per-day
	// check in sealAndUploadDays then waits for that day's whole backlog.
	mayseal := false
	if now.Before(r.holdMinutes) {
		r.opt.Logger.DebugContext(ctx, "minute uploads on hold after a refused batch", "until", r.holdMinutes)
	} else {
		drained, err := r.uploadMinutes(ctx, t.Target, dev, now)
		var refused *refusedError
		switch {
		case errors.As(err, &refused):
			// Not a transport problem: retrying soon won't help, and the
			// minutes are kept (not marked sent) for when it does.
			r.holdMinutes = now.Add(r.opt.Reprobe)
			r.opt.Logger.ErrorContext(ctx, "server refused credited minutes; holding them and retrying later",
				"err", refused.err, "retry_at", r.holdMinutes)
			r.show(ctx, keyRefused, refusedText(refused.err))
		case err != nil:
			r.fail(ctx, t, err)
			return
		default:
			mayseal = true
			if !drained {
				r.opt.Logger.InfoContext(ctx, "minute upload budget used up; the rest goes next run",
					"batches", maxBatchesPerRun)
			}
			r.show(ctx, keyRefused, "")
		}
	}
	err = r.sealAndUploadDays(ctx, t.Target, dev, now, mayseal)
	if isServerError(err) {
		r.fail(ctx, t, err)
		return
	}
	// A local problem sealing a day (reading the database, the active-time
	// source, encoding) is not the server's: it is shown and retried on the
	// next run without backing off the uploads.
	local := ""
	if err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.ErrorContext(ctx, "sealing evidence day", "err", err)
		}
		local = err.Error()
	}
	r.show(ctx, keyEvidenceLocal, local)
	r.succeed(ctx, t.Target, now)
}

// refusedError is a minutes batch the server refused as a whole for a
// reason other than its size (such as an unknown rules version).
type refusedError struct{ err error }

func (e *refusedError) Error() string { return e.err.Error() }
func (e *refusedError) Unwrap() error { return e.err }

func refusedText(err error) string {
	return "the server refused credited minutes (" + serverMessage(err) +
		"); they are kept and retried later. Updating Logbook may help."
}

// isServerError reports whether err came from the server API (and so
// drives backoff), as opposed to local storage or computation.
func isServerError(err error) bool {
	var e *serverapi.Error
	return errors.As(err, &e)
}

// show persists a status value when it differs from what was last written.
func (r *Runner) show(ctx context.Context, key, value string) {
	if old, ok := r.shown[key]; ok && old == value {
		return
	}
	if r.shown == nil {
		r.shown = map[string]string{}
	}
	if err := r.opt.Store.SetMany(ctx, localdb.KV{Key: key, Value: value}); err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.WarnContext(ctx, "saving crediting state", "err", err)
		}
		return
	}
	r.shown[key] = value
}

// prune drops old local rows.
func (r *Runner) prune(ctx context.Context) {
	res, err := r.opt.Store.PruneCrediting(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.WarnContext(ctx, "pruning credited minutes and heartbeats", "err", err)
		}
		return
	}
	if res != (localdb.CreditPruneResult{}) {
		r.opt.Logger.InfoContext(ctx, "pruned old local crediting data", "heartbeats", res.Heartbeats,
			"credited_minutes", res.CreditedMinutes)
	}
}

func (r *Runner) persist(ctx context.Context, kvs ...localdb.KV) {
	if err := r.opt.Store.SetMany(ctx, kvs...); err != nil && ctx.Err() == nil {
		r.opt.Logger.WarnContext(ctx, "saving crediting state", "err", err)
	}
}
