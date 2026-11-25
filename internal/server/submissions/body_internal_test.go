package submissions

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// The upload gate bounds the memory uploads take: one upload per user at a
// time (429), and a body waits for a decode slot until uploadWait, then
// gets 503. Both say when to retry.
func TestUploadGate(t *testing.T) {
	t.Parallel()
	g := newUploadGate(1, 20*time.Millisecond)
	alice, bob := uuid.New(), uuid.New()
	status := func(err error) int {
		var he *httpapi.Error
		if !errors.As(err, &he) {
			t.Fatalf("error = %v, want an *httpapi.Error", err)
		}
		return he.Status
	}

	leave, err := g.enter(httptest.NewRecorder(), alice)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if _, err := g.enter(w, alice); status(err) != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("second upload of alice = %v, Retry-After %q", err, w.Header().Get("Retry-After"))
	}
	leaveBob, err := g.enter(httptest.NewRecorder(), bob)
	if err != nil {
		t.Fatalf("bob while alice uploads: %v", err)
	}

	release, err := g.slot(t.Context(), httptest.NewRecorder())
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	if _, err := g.slot(t.Context(), w); status(err) != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("slot while the only one is taken = %v, Retry-After %q", err, w.Header().Get("Retry-After"))
	}
	release()
	release, err = g.slot(t.Context(), httptest.NewRecorder())
	if err != nil {
		t.Fatalf("slot after release: %v", err)
	}
	release()

	leave()
	leaveBob()
	leave, err = g.enter(httptest.NewRecorder(), alice)
	if err != nil {
		t.Fatalf("alice after her upload ended: %v", err)
	}
	leave()
}
