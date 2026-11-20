package upload

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

func TestWorkerKeepsQueueOnWholeRequestErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler func(w http.ResponseWriter, hbs []wire.Heartbeat)
	}{
		{"404 from a stale api_url", func(w http.ResponseWriter, _ []wire.Heartbeat) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}},
		{"405 during a deploy", func(w http.ResponseWriter, _ []wire.Heartbeat) { w.WriteHeader(http.StatusMethodNotAllowed) }},
		{"408 from a proxy", func(w http.ResponseWriter, _ []wire.Heartbeat) { w.WriteHeader(http.StatusRequestTimeout) }},
		{"captive portal 200", func(w http.ResponseWriter, _ []wire.Heartbeat) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html>Log in to school wifi</html>`))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, 10)
			h.enqueue(t, 25)
			h.srv.handler = tt.handler
			if wait := h.w.cycle(ctx); wait <= 0 || wait == DefaultInterval {
				t.Fatalf("wait = %v, want a backoff", wait)
			}
			if n := h.pending(t); n != 25 {
				t.Fatalf("pending = %d, want 25 (nothing deleted)", n)
			}
			if len(h.srv.batches) != 1 {
				t.Fatalf("requests = %d, want 1 (stop at the first failure)", len(h.srv.batches))
			}
			st, _ := LoadState(ctx, h.db, "logbook")
			if !st.LastSuccess.IsZero() || st.LastError == "" || st.Rejected != 0 {
				t.Fatalf("state = %+v, want an error and no success", st)
			}
		})
	}
}

func TestWorkerSplitsRefusedBatchAndDropsOnlyTheBadItem(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	h.enqueue(t, 5)
	const poison = "/src/f2.go"
	h.srv.handler = func(w http.ResponseWriter, hbs []wire.Heartbeat) {
		for _, hb := range hbs {
			if hb.Entity == poison {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid batch"}`))
				return
			}
		}
		acceptAll(w, hbs)
	}
	if wait := h.w.cycle(ctx); wait <= 0 || wait == DefaultInterval {
		t.Fatalf("wait = %v, want a backoff after dropping a single item", wait)
	}
	h.w.cycle(ctx)
	if n := h.pending(t); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	st, _ := LoadState(ctx, h.db, "logbook")
	if st.Rejected != 1 {
		t.Fatalf("rejected = %d, want only the bad item", st.Rejected)
	}
	if h.w.batchSize() == 1 {
		t.Fatal("batch size did not grow back after successes")
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	for _, b := range h.srv.batches {
		if len(b) == 1 && b[0].Entity == poison {
			return
		}
	}
	t.Fatal("the bad item was never sent on its own")
}

func TestNewWorkerClearsStalePersistedState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	// Left by an earlier run whose key was rejected.
	if err := h.db.SetMany(ctx,
		localdb.KV{Key: stateKey("logbook", keyPaused), Value: ReasonReLogin},
		localdb.KV{Key: stateKey("logbook", keyLastError), Value: "server answered 401: re-login needed"},
		localdb.KV{Key: stateKey("logbook", keyLastErrorAt), Value: "2026-11-01T00:00:00Z"},
	); err != nil {
		t.Fatal(err)
	}
	h.w.cycle(ctx) // empty queue: the pause reason is still rewritten
	if st, _ := LoadState(ctx, h.db, "logbook"); st.Paused != "" {
		t.Fatalf("stale pause survived a restart: %+v", st)
	}
	h.enqueue(t, 2)
	h.w.cycle(ctx)
	st, _ := LoadState(ctx, h.db, "logbook")
	if st.Paused != "" || st.LastError != "" || !st.LastErrorAt.IsZero() || st.LastSuccess.IsZero() {
		t.Fatalf("state after a successful upload = %+v", st)
	}
}

func TestWorkerReprobesAfterForbidden(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	clock := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	h.w.opt.Now = func() time.Time { return clock }
	h.enqueue(t, 3)
	h.srv.handler = func(w http.ResponseWriter, _ []wire.Heartbeat) { w.WriteHeader(http.StatusForbidden) }
	h.w.cycle(ctx)
	clock = clock.Add(DefaultReprobe - time.Minute)
	h.w.cycle(ctx)
	if len(h.srv.batches) != 1 {
		t.Fatalf("requests before the re-probe = %d, want 1", len(h.srv.batches))
	}

	// The school filter is gone: the same key works again.
	h.srv.mu.Lock()
	h.srv.handler = acceptAll
	h.srv.mu.Unlock()
	clock = clock.Add(2 * time.Minute)
	h.w.cycle(ctx)
	if n := h.pending(t); n != 0 {
		t.Fatalf("pending after re-probe = %d", n)
	}
	if st, _ := LoadState(ctx, h.db, "logbook"); st.Paused != "" || h.w.State().Paused != "" {
		t.Fatalf("still paused after an accepted re-probe: %+v", st)
	}
}

func TestWorkerStaysPausedWhenReprobeIsRejected(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	clock := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	h.w.opt.Now = func() time.Time { return clock }
	h.enqueue(t, 1)
	h.srv.handler = func(w http.ResponseWriter, _ []wire.Heartbeat) { w.WriteHeader(http.StatusUnauthorized) }
	h.w.cycle(ctx)
	clock = clock.Add(DefaultReprobe)
	h.w.cycle(ctx) // re-probe, rejected again
	h.w.cycle(ctx) // paused for another interval
	if len(h.srv.batches) != 2 {
		t.Fatalf("requests = %d, want 2", len(h.srv.batches))
	}
	if st := h.w.State(); st.Paused != ReasonReLogin {
		t.Fatalf("state = %+v", st)
	}
	if n := h.pending(t); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
}
