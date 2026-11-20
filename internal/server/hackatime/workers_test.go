package hackatime

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/apneduniya/logbook/internal/server/jobs"
)

// TestImportJobEndToEnd links through HTTP, then lets a real River client
// work the queued import job until the history is in the sink.
func TestImportJobEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "green")
	for d := range 3 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(d)*24*time.Hour+time.Hour), "/x.go", nil)
	}

	workers := river.NewWorkers()
	tr := NewTrustRefresher(e.st, e.box, e.client, e.logger, e.clock)
	if err := AddWorkers(workers, e.importer(t, ImportOptions{}), tr); err != nil {
		t.Fatal(err)
	}
	jc, err := jobs.New(e.st.Pool, jobs.Options{
		Workers: workers, PeriodicJobs: PeriodicJobs(), Queues: map[string]int{Queue: 2}, Logger: e.logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Link with the real client so the job is inserted the production way.
	e.svc = NewService(e.st, e.box, e.client, jc.River(), ServiceOptions{
		Logger: e.logger, ImportFrom: day1, Now: e.clock,
	})
	e.mux = http.NewServeMux()
	NewHandler(e.svc, stubSession, HandlerOptions{}).Register(e.mux)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events, unsubscribe := jc.River().Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	if err := jc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := jc.Stop(stopCtx); err != nil {
			t.Errorf("Stop() err = %v", err)
		}
	})

	alice := e.newUser(t, "alice")
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("link = %d %s", rec.Code, rec.Body)
	}
	for {
		select {
		case ev := <-events:
			if ev.Job.Kind != (ImportArgs{}).Kind() {
				continue
			}
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("import job ended as %s", ev.Kind)
			}
			if e.sink.count(alice) != 3 {
				t.Fatalf("imported %d, want 3", e.sink.count(alice))
			}
			imp, err := e.st.GetHackatimeImport(ctx, alice)
			if err != nil || imp.Status != importDone {
				t.Fatalf("import = %+v, %v", imp, err)
			}
			return
		case <-ctx.Done():
			t.Fatal("import job did not finish")
		}
	}
}
