package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Options configures the job client.
type Options struct {
	// Workers holds every worker the client may run. Required.
	Workers *river.Workers
	// PeriodicJobs are scheduled while this process is the River leader.
	PeriodicJobs []*river.PeriodicJob
	// Queues maps queue name to its worker concurrency. The default queue is
	// always present; feature queues (for example "hackatime") are added here.
	Queues map[string]int
	// Logger receives River's logs. Defaults to slog.Default().
	Logger *slog.Logger
	// JobTimeout is the default per-job timeout. Workers may override it.
	JobTimeout time.Duration
}

// DefaultConcurrency is the default queue's worker count.
const DefaultConcurrency = 10

// Client runs background jobs.
type Client struct {
	river *river.Client[pgx.Tx]
}

// New builds a job client on pool. It does not start working jobs; call Start.
func New(pool *pgxpool.Pool, opts Options) (*Client, error) {
	if pool == nil {
		return nil, errors.New("jobs: pool is required")
	}
	if opts.Workers == nil {
		return nil, errors.New("jobs: workers are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	queues := map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: DefaultConcurrency}}
	for name, n := range opts.Queues {
		if n < 1 {
			return nil, fmt.Errorf("jobs: queue %q needs at least one worker", name)
		}
		queues[name] = river.QueueConfig{MaxWorkers: n}
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:       logger,
		Queues:       queues,
		Workers:      opts.Workers,
		PeriodicJobs: opts.PeriodicJobs,
		JobTimeout:   opts.JobTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: creating river client: %w", err)
	}
	return &Client{river: rc}, nil
}

// River exposes the underlying client so feature services can enqueue jobs
// (Insert / InsertTx) through their own small interfaces.
func (c *Client) River() *river.Client[pgx.Tx] { return c.river }

// Start begins fetching and working jobs. It returns once River is running;
// work continues in the background until Stop.
func (c *Client) Start(ctx context.Context) error {
	if err := c.river.Start(ctx); err != nil {
		return fmt.Errorf("jobs: starting: %w", err)
	}
	return nil
}

// Stop stops fetching new jobs and waits for running ones until ctx is done,
// then cancels whatever is still running.
func (c *Client) Stop(ctx context.Context) error {
	err := c.river.Stop(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("jobs: stopping: %w", err)
	}
	// Soft stop ran out of time: cancel running jobs. They resume from their
	// persisted progress when retried.
	hardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.river.StopAndCancel(hardCtx); err != nil {
		return fmt.Errorf("jobs: stopping and cancelling: %w", err)
	}
	return nil
}

// Migrate brings River's own tables (river_job, river_leader, ...) up to the
// version this binary expects. It is safe to run on every start.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("jobs: pool is required")
	}
	m, err := newMigrator(pool)
	if err != nil {
		return err
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("jobs: migrating river schema: %w", err)
	}
	return nil
}

// MigrationVersion is the latest River schema version bundled in this
// binary. Test helpers use it to key template databases.
func MigrationVersion() (int, error) {
	m, err := newMigrator(nil)
	if err != nil {
		return 0, err
	}
	all := m.AllVersions()
	if len(all) == 0 {
		return 0, errors.New("jobs: river has no migrations")
	}
	return all[len(all)-1].Version, nil
}

// newMigrator builds River's migrator. A nil pool is allowed for reading
// bundled versions only. Migration progress isn't logged; Migrate's error is
// the signal that matters.
func newMigrator(pool *pgxpool.Pool) (*rivermigrate.Migrator[pgx.Tx], error) {
	m, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		return nil, fmt.Errorf("jobs: creating river migrator: %w", err)
	}
	return m, nil
}
