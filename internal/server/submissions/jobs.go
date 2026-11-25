package submissions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// Queue is the River queue of the re-check and retention jobs.
const Queue = "submissions"

// verifyTimeout bounds one re-check: 92 days on several devices is a few
// hundred thousand rows and as many replays.
const verifyTimeout = 15 * time.Minute

// VerifyArgs queues the re-check of one submission. Jobs aren't unique: a
// part added while a re-check runs queues another, and only the run that
// read the current parts writes its result (parts_version).
type VerifyArgs struct {
	SubmissionID uuid.UUID `json:"submission_id"`
}

// Kind implements river.JobArgs.
func (VerifyArgs) Kind() string { return "submission_verify" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (VerifyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: Queue, MaxAttempts: 10}
}

// RetentionArgs is the periodic job that deletes expired raw rows.
type RetentionArgs struct{}

// Kind implements river.JobArgs.
func (RetentionArgs) Kind() string { return "submission_raw_retention" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (RetentionArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: Queue} }

// VerifyWorker runs Checker.Verify.
type VerifyWorker struct {
	river.WorkerDefaults[VerifyArgs]
	c *Checker
}

// Timeout implements river.Worker.
func (w *VerifyWorker) Timeout(*river.Job[VerifyArgs]) time.Duration { return verifyTimeout }

// Work implements river.Worker.
func (w *VerifyWorker) Work(ctx context.Context, job *river.Job[VerifyArgs]) error {
	return w.c.Verify(ctx, job.Args.SubmissionID)
}

// RetentionWorker runs Checker.ExpireRaw.
type RetentionWorker struct {
	river.WorkerDefaults[RetentionArgs]
	c *Checker
}

// Work implements river.Worker.
func (w *RetentionWorker) Work(ctx context.Context, _ *river.Job[RetentionArgs]) error {
	_, err := w.c.ExpireRaw(ctx)
	return err
}

// AddWorkers registers the submission workers.
func AddWorkers(workers *river.Workers, c *Checker) error {
	if err := river.AddWorkerSafely(workers, &VerifyWorker{c: c}); err != nil {
		return fmt.Errorf("registering submission workers: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &RetentionWorker{c: c}); err != nil {
		return fmt.Errorf("registering submission workers: %w", err)
	}
	return nil
}

// PeriodicJobs returns the retention sweep, every 6 hours.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(6*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return RetentionArgs{}, nil },
			&river.PeriodicJobOpts{ID: "submission_raw_retention", RunOnStart: true},
		),
	}
}
