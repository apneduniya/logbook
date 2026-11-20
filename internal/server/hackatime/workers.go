package hackatime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue is the River queue for Hackatime jobs. Keep its concurrency low:
// every job shares the server's Hackatime rate limit.
const Queue = "hackatime"

// importMaxAttempts bounds retries of one import before it is marked failed.
const importMaxAttempts = 12

// activeStates makes jobs unique only while queued or running, so a finished
// import can be queued again when the user relinks.
func activeStates() []rivertype.JobState {
	return []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
		rivertype.JobStateRunning, rivertype.JobStateScheduled,
	}
}

// ImportArgs queues a history import for one user.
type ImportArgs struct {
	UserID uuid.UUID `json:"user_id"`
}

// Kind implements river.JobArgs.
func (ImportArgs) Kind() string { return "hackatime_import" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ImportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       Queue,
		MaxAttempts: importMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: activeStates()},
	}
}

// TrustRefreshArgs queues a trust level refresh for one user.
type TrustRefreshArgs struct {
	UserID uuid.UUID `json:"user_id"`
}

// Kind implements river.JobArgs.
func (TrustRefreshArgs) Kind() string { return "hackatime_trust_refresh" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (TrustRefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       Queue,
		MaxAttempts: 5,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: activeStates()},
	}
}

// TrustSweepArgs is the periodic job that queues refreshes for stale links.
type TrustSweepArgs struct{}

// Kind implements river.JobArgs.
func (TrustSweepArgs) Kind() string { return "hackatime_trust_sweep" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (TrustSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByState: activeStates()}}
}

// ImportWorker runs Importer.Run as a River job.
type ImportWorker struct {
	river.WorkerDefaults[ImportArgs]
	im *Importer
}

// Timeout leaves room for the run budget plus one slow day.
func (w *ImportWorker) Timeout(*river.Job[ImportArgs]) time.Duration {
	return w.im.budget + 5*time.Minute
}

// Work imports until done or the run budget is spent, then reschedules itself.
func (w *ImportWorker) Work(ctx context.Context, job *river.Job[ImportArgs]) error {
	res, err := w.im.Run(ctx, job.Args.UserID, job.Attempt >= job.MaxAttempts)
	var rl *RateLimitedError
	switch {
	case errors.As(err, &rl):
		return river.JobSnooze(rl.RetryAfter)
	case err != nil && res.Done:
		return river.JobCancel(err) // marked failed; retrying won't help
	case err != nil:
		return err
	case !res.Done:
		return river.JobSnooze(0) // budget spent; continue from the saved day
	}
	return nil
}

// TrustRefreshWorker refreshes one user's trust level.
type TrustRefreshWorker struct {
	river.WorkerDefaults[TrustRefreshArgs]
	r *TrustRefresher
}

// Work implements river.Worker.
func (w *TrustRefreshWorker) Work(ctx context.Context, job *river.Job[TrustRefreshArgs]) error {
	err := w.r.Refresh(ctx, job.Args.UserID)
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		return river.JobSnooze(rl.RetryAfter)
	}
	return err
}

// TrustSweepWorker queues a refresh job for every link due a trust check.
type TrustSweepWorker struct {
	river.WorkerDefaults[TrustSweepArgs]
	r *TrustRefresher
}

// Work implements river.Worker.
func (w *TrustSweepWorker) Work(ctx context.Context, _ *river.Job[TrustSweepArgs]) error {
	ids, err := w.r.DueUsers(ctx)
	if err != nil || len(ids) == 0 {
		return err
	}
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("getting river client: %w", err)
	}
	params := make([]river.InsertManyParams, len(ids))
	for i, id := range ids {
		params[i] = river.InsertManyParams{Args: TrustRefreshArgs{UserID: id}}
	}
	if _, err := client.InsertMany(ctx, params); err != nil {
		return fmt.Errorf("queueing hackatime trust refreshes: %w", err)
	}
	return nil
}

// AddWorkers registers the Hackatime workers.
func AddWorkers(workers *river.Workers, im *Importer, tr *TrustRefresher) error {
	for _, add := range []func() error{
		func() error { return river.AddWorkerSafely(workers, &ImportWorker{im: im}) },
		func() error { return river.AddWorkerSafely(workers, &TrustRefreshWorker{r: tr}) },
		func() error { return river.AddWorkerSafely(workers, &TrustSweepWorker{r: tr}) },
	} {
		if err := add(); err != nil {
			return fmt.Errorf("registering hackatime workers: %w", err)
		}
	}
	return nil
}

// PeriodicJobs returns the Hackatime schedules: an hourly sweep that queues
// trust refreshes for links not checked in TrustMaxAge, so each linked user is
// refreshed about daily without one long job.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return TrustSweepArgs{}, nil },
			&river.PeriodicJobOpts{ID: "hackatime_trust_sweep", RunOnStart: true},
		),
	}
}
