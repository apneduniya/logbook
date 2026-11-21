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
	// drainWait bounds how long Run waits for the startup spool drain
	// before its first run.
	drainWait = 2 * time.Minute
)

// Store is the local storage the Runner needs. *localdb.DB satisfies it.
type Store interface {
	View(ctx context.Context, fn func(*localdb.Snapshot) error) error
	RecordRepos(ctx context.Context, at time.Time, repos []struggle.Repo) error
	SaveRun(ctx context.Context, r localdb.Run) (localdb.RunResult, error)
	AdoptMinute(ctx context.Context, minute time.Time, rows []localdb.CreditedMinute) error
	UnsentMinutes(ctx context.Context, after, holdOpenBefore time.Time, limit int) ([]localdb.CreditedMinute, error)
	MarkMinutesTried(ctx context.Context, ms []localdb.CreditedMinute) error
	MarkMinutesSent(ctx context.Context, a localdb.MinutesAnswer) error
	RetireMinutesBefore(ctx context.Context, t time.Time) error
	UnsentMinuteCount(ctx context.Context) (int64, error)
	MinutesPendingBefore(ctx context.Context, t time.Time) (int64, error)
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
// are logged as repo mapping rows (ADR 0017), and each run reads the
// mapping from its own snapshot of the log.
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
	// Settle is how long after the Runner starts, or after a wake or a gap
	// in its runs, it waits before making minutes final (DefaultSettle).
	// Negative means no wait (tests).
	Settle time.Duration
	// Drained is closed once the daemon's startup spool drain has finished.
	// Run waits for it (at most drainWait) before its first run, and no
	// minute becomes final before it is closed. nil means there is no
	// spool to wait for.
	Drained <-chan struct{}

	Interval, MinBackoff, MaxBackoff, Reprobe time.Duration
	// Batch is the minutes per upload (serverapi.MaxMinutes).
	Batch int
	// Jitter returns a value in [0, 1).
	Jitter func() float64
}

// Evidence blocks are read and hashed in chunks of sealChunk sequence
// numbers, at most sealChunks per run (sealDay).
const (
	defaultSealChunk  = 20_000
	defaultSealChunks = 25
)

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
	// ahead is how far the system clock was ahead of the server's at the
	// latest server time seen since the agent started or woke (0 when
	// behind); aheadKnown is set once one was seen. No minute becomes
	// final without it, and finality goes by the system clock minus ahead,
	// so a clock running ahead never makes future minutes final.
	ahead      time.Duration
	aheadKnown bool
	// upSince is when this run of the agent began receiving events: the
	// Runner's first run, or the first run after a wake or a gap. lastRun
	// is the previous run's clock, with its monotonic reading.
	upSince, lastRun time.Time
	// late is the log's high-water mark up to which rows have been checked
	// for lateness (lateKnown once read or set).
	late      int64
	lateKnown bool

	// sealChunk and sealChunks bound one run's evidence hashing (tests
	// make them small).
	sealChunk  int64
	sealChunks int

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
	if o.Settle == 0 {
		o.Settle = DefaultSettle
	}
	return &Runner{opt: o, batch: o.Batch, sealChunk: defaultSealChunk, sealChunks: defaultSealChunks}
}

// Run runs the pipeline every Interval (a few seconds past each minute with
// the default interval) until ctx is done. It always returns nil. The first
// run waits for the startup spool drain (Options.Drained), at most
// drainWait, so it starts from every event the hooks spooled while the
// agent was down.
func (r *Runner) Run(ctx context.Context) error {
	if r.opt.Drained != nil {
		wait := time.NewTimer(drainWait)
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil
		case <-r.opt.Drained:
		case <-wait.C:
			r.opt.Logger.WarnContext(ctx, "the hook spool is still draining; crediting starts without making minutes final")
		}
		wait.Stop()
	}
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
	st := r.settle(ctx, now)
	if err := r.compute(ctx, now, through, st); err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.ErrorContext(ctx, "computing credited minutes", "err", err)
		}
		return
	}
	r.upload(ctx, now, st)
	if n, err := r.opt.Store.UnsentMinuteCount(ctx); err == nil {
		r.persist(ctx, localdb.KV{Key: keyWaiting, Value: strconv.FormatInt(n, 10)})
	}
}

// compute runs the engine once at the run's key and stores the result
// (ADR 0017). The key is (T, H): T is the end of the window, the whole
// minute now is in, and H the log's high-water mark in the run's one
// snapshot, from which it reads its events and its repo mapping. A write
// not committed by then is outside the run entirely. When the agent has
// settled, the minutes up to T - FinalLead become final in the same write.
// Minutes before through (the end of the last sealed day) are never
// stored: the engine still reads the whole window, so the rest come out
// the same.
func (r *Runner) compute(ctx context.Context, now, through time.Time, st settleState) error {
	s := spanAt(now)
	r.recordRepos(ctx, now)
	var (
		key   localdb.Key
		repos []struggle.Repo
		late  localdb.LateResult
	)
	below := false
	err := r.opt.Store.View(ctx, func(snap *localdb.Snapshot) error {
		if s.history.Before(snap.Floor) {
			// The run would read history that pruning or the row cap has
			// deleted, which the sealed blocks still hold: a replay at its
			// key would see rows it didn't. Only a clock set back far can
			// get here; wait for it to catch up.
			below = true
			return nil
		}
		key = localdb.Key{T: s.to, H: snap.H}
		if err := r.cache.refresh(ctx, snap, s.history, s.to); err != nil {
			return err
		}
		var err error
		if repos, err = snap.Repos(ctx, snap.H); err != nil {
			return err
		}
		late, err = r.lateRows(ctx, snap)
		return err
	})
	if err != nil {
		return err
	}
	if below {
		msg := "the system clock is set back before deleted history, so no minutes are computed until it catches up"
		if r.shown[keyBelowFloor] != msg {
			r.opt.Logger.WarnContext(ctx, msg, "window_start", s.history.UTC().Format(time.RFC3339))
		}
		r.show(ctx, keyBelowFloor, msg)
		return nil
	}
	r.show(ctx, keyBelowFloor, "")
	var active []struggle.ActiveTime
	if r.opt.ActiveTimes != nil {
		if active, err = r.opt.ActiveTimes.ActiveTimes(ctx, s.history, s.to); err != nil {
			// Optional signal, and it feeds only flags: credit without it
			// rather than not at all.
			r.opt.Logger.WarnContext(ctx, "reading active time; crediting without it", "err", err)
			active = nil
		}
	}
	res := r.opt.Compute(engineInput(s, r.cache.before(s.to), active, repos, r.opt.TZ))
	keep := s
	if keep.from.Before(through) {
		keep.from = through
	}
	if keep.to.After(keep.from) {
		run := localdb.Run{Key: key, From: keep.from, To: keep.to, Minutes: minuteRows(keep, res), Removal: removal}
		if st.settled {
			run.FinalBefore = finalBefore(s.to.Add(-r.ahead))
		}
		saved, err := r.opt.Store.SaveRun(ctx, run)
		if err != nil {
			return err
		}
		r.opt.Logger.DebugContext(ctx, "credited minutes computed", "t", key.T.Format(time.RFC3339), "h", key.H,
			"replaced", saved.Replaced, "kept", saved.Kept, "finalized", saved.Finalized)
	}
	r.persist(ctx, localdb.KV{Key: keyLastComputed, Value: formatTime(now)})
	r.countLate(ctx, late, key.H)
	return nil
}

// recordRepos logs the active repos as repo mapping rows, before the run's
// snapshot. If the source fails, the mapping already logged stays in force.
func (r *Runner) recordRepos(ctx context.Context, now time.Time) {
	if r.opt.Repos == nil {
		return
	}
	repos, err := r.opt.Repos.ActiveRepos(ctx)
	if err == nil {
		err = r.opt.Store.RecordRepos(ctx, now, repos)
	}
	if err != nil && ctx.Err() == nil {
		r.opt.Logger.WarnContext(ctx, "recording active repos; using the last known mapping", "err", err)
	}
}

// upload registers the device if needed, then sends minutes and evidence.
func (r *Runner) upload(ctx context.Context, now time.Time, st settleState) {
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
	// The device the stored minutes belong to is recorded (keyDevice) only
	// once the switch to it is complete, so a crash after the identity
	// persisted a replacement but before its minutes were retired is
	// caught on the next run, when Ensure no longer reports Replaced.
	owner, err := r.opt.Store.Get(ctx, keyDevice)
	if err != nil {
		r.opt.Logger.ErrorContext(ctx, "reading the device credited minutes belong to", "err", err)
		return
	}
	if dev.Replaced || (owner != "" && owner != dev.ID) {
		// A new device starts clean: minutes from before it registered
		// belong to the previous device and are not sent again.
		if err := r.opt.Store.RetireMinutesBefore(ctx, dev.RegisteredAt.Truncate(time.Minute)); err != nil {
			r.opt.Logger.ErrorContext(ctx, "retiring minutes of the previous device", "err", err)
			return
		}
		// Its evidence chain starts over too, from the oldest retained row,
		// and until it seals a block no row counts as sealed.
		if err := r.opt.Store.SetMany(ctx, localdb.KV{Key: keyEvidenceDevice, Value: dev.ID}, localdb.KV{Key: keyEvidenceDay, Value: ""},
			localdb.KV{Key: keyEvidenceHash, Value: ""}, localdb.KV{Key: keyEvidenceState, Value: ""},
			localdb.KV{Key: keyEvidenceError, Value: ""}, localdb.KV{Key: localdb.KeySealed, Value: "0"},
			localdb.KV{Key: keyDevice, Value: dev.ID}); err != nil {
			r.opt.Logger.ErrorContext(ctx, "starting the new device's evidence chain", "err", err)
			return
		}
	} else if owner == "" {
		// The first device, or an agent from before keyDevice: these
		// minutes are its own.
		if err := r.opt.Store.SetMany(ctx, localdb.KV{Key: keyDevice, Value: dev.ID}); err != nil {
			r.opt.Logger.ErrorContext(ctx, "recording the device credited minutes belong to", "err", err)
			return
		}
	}
	// Sealing a day closes it to minutes on the server, so new days are
	// sealed only on a run whose minute uploads went through; the per-day
	// check in sealAndUploadDays then waits for that day's whole backlog.
	mayseal := false
	if now.Before(r.holdMinutes) {
		r.opt.Logger.DebugContext(ctx, "minute uploads on hold after a refused batch", "until", r.holdMinutes)
	} else {
		drained, err := r.uploadMinutes(ctx, t.Target, dev, now, st)
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
	if mayseal && !r.aheadKnown {
		// No minute becomes final before the server's clock has been seen
		// since the agent started or woke: ask for it if no upload brought it.
		if err := r.askServerTime(ctx, t.Target, dev); err != nil {
			r.fail(ctx, t, err)
			return
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
