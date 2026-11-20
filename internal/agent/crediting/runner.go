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
	MarkRevoked(ctx context.Context, t serverapi.Target) error
	Forget(ctx context.Context) error
}

// ActiveTimeSource returns Claude Code active-time datapoints in [from, to).
// It is optional.
type ActiveTimeSource interface {
	ActiveTimes(ctx context.Context, from, to time.Time) ([]struggle.ActiveTime, error)
}

// RepoSource lists recently active repositories. It is optional.
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
	cache   eventCache // used only while running is held

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
	if err := r.compute(ctx, now); err != nil {
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

// compute runs the engine over the window and stores the result.
func (r *Runner) compute(ctx context.Context, now time.Time) error {
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
	var repos []struggle.Repo
	if r.opt.Repos != nil {
		if repos, err = r.opt.Repos.ActiveRepos(ctx); err != nil {
			r.opt.Logger.WarnContext(ctx, "listing active repos; crediting without them", "err", err)
			repos = nil
		}
	}
	res := r.opt.Compute(engineInput(s, ev, active, repos, r.opt.TZ))
	saved, err := r.opt.Store.SaveMinutes(ctx, localdb.MinuteUpdate{
		From: s.from, To: s.to, OpenFrom: s.finalBefore, Minutes: minuteRows(s, res), Superseded: superseded,
	})
	if err != nil {
		return err
	}
	r.opt.Logger.DebugContext(ctx, "credited minutes computed", "inserted", saved.Inserted,
		"updated", saved.Updated, "deleted", saved.Deleted)
	r.persist(ctx, localdb.KV{Key: keyLastComputed, Value: formatTime(now)})
	return nil
}

// upload registers the device if needed, then sends minutes and evidence.
func (r *Runner) upload(ctx context.Context, now time.Time) {
	t := r.opt.Target()
	if !r.ready(ctx, t, now) {
		return
	}
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
	}
	if err := r.uploadMinutes(ctx, t.Target, dev, now); err != nil {
		r.fail(ctx, t, err)
		return
	}
	if err := r.sealAndUploadDays(ctx, t.Target, dev, now); err != nil {
		r.fail(ctx, t, err)
		return
	}
	r.succeed(ctx, now)
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
