package submissions_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/submissions"
)

// The re-check reads, decodes and verifies one part at a time, dropping
// each before the next, and only within the decode slots it shares with
// uploads: while the only slot is taken, the job waits (decoding nothing)
// and an upload gets 503; once it is free the job finishes as usual.
func TestRecheckDecodesOnePartAtATimeInSharedSlots(t *testing.T) {
	t.Parallel()
	w := newWorld(t) // one decode slot, shared by the Service and the Checker
	created, _ := w.submit("terra")
	part := mustJSON(t, map[string]any{"device_id": w.b.id.String(), "blocks": []any{}, "minutes": []any{}})
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + created.ID.String() + "/parts", user: w.alice,
		body: part, gz: true}, http.StatusAccepted)

	var mu sync.Mutex
	held, most, decoded := 0, 0, 0
	submissions.SetDecodeHook(w.checker, func(delta int) {
		mu.Lock()
		defer mu.Unlock()
		held += delta
		if delta > 0 {
			decoded++
		}
		most = max(most, held)
	})

	release, err := submissions.AcquireSlot(t.Context(), w.slots)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.checker.Verify(t.Context(), created.ID) }()
	select {
	case err := <-done:
		release()
		t.Fatalf("the re-check ran while the only decode slot was taken: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	early := decoded
	mu.Unlock()
	if early != 0 {
		release()
		t.Fatalf("%d parts decoded while the only decode slot was taken", early)
	}
	// An upload waits for the same slot and is refused when none frees up.
	w.uploadWait = 50 * time.Millisecond
	w.resetLimits()
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: w.upload("terra"), gz: true},
		http.StatusServiceUnavailable)
	release()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if decoded != 2 || most != 1 || held != 0 {
		t.Fatalf("decoded %d parts, at most %d held at once, %d still held; want 2, 1, 0", decoded, most, held)
	}
	if s := w.status(created.ID); s != "ready" {
		t.Fatalf("status = %s", s)
	}
	if n := w.count(`SELECT count(*) FROM submission_minutes WHERE submission_id = $1 AND result IS NULL`, created.ID); n != 0 {
		t.Fatalf("%d frozen rows without a result", n)
	}
}
