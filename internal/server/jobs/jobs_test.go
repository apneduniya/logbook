package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/apneduniya/logbook/internal/server/jobs"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

type pingArgs struct {
	N int `json:"n"`
}

func (pingArgs) Kind() string { return "test_ping" }

type pingWorker struct {
	river.WorkerDefaults[pingArgs]
	done chan int
}

func (w *pingWorker) Work(_ context.Context, job *river.Job[pingArgs]) error {
	w.done <- job.Args.N
	return nil
}

func TestMigrationVersion(t *testing.T) {
	v, err := jobs.MigrationVersion()
	if err != nil || v < 1 {
		t.Fatalf("MigrationVersion() = %d, %v", v, err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := storetest.New(t) // template already includes River's tables
	ctx := context.Background()
	if err := jobs.Migrate(ctx, st.Pool); err != nil {
		t.Fatalf("Migrate() err = %v", err)
	}
	var exists bool
	if err := st.Pool.QueryRow(ctx, "SELECT to_regclass('river_job') IS NOT NULL").Scan(&exists); err != nil || !exists {
		t.Fatalf("river_job missing: %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	st := storetest.New(t)
	tests := []struct {
		name string
		opts jobs.Options
	}{
		{"no workers", jobs.Options{}},
		{"zero concurrency queue", jobs.Options{Workers: river.NewWorkers(), Queues: map[string]int{"x": 0}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := jobs.New(st.Pool, tt.opts); err == nil {
				t.Fatal("New() err = nil")
			}
		})
	}
	if _, err := jobs.New(nil, jobs.Options{Workers: river.NewWorkers()}); err == nil {
		t.Fatal("New(nil pool) err = nil")
	}
}

func TestStartWorkStop(t *testing.T) {
	st := storetest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := &pingWorker{done: make(chan int, 1)}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	c, err := jobs.New(st.Pool, jobs.Options{Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.River().Insert(ctx, pingArgs{N: 7}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-w.done:
		if n != 7 {
			t.Fatalf("worked %d, want 7", n)
		}
	case <-ctx.Done():
		t.Fatal("job not worked")
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop() err = %v", err)
	}
}
