package live_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

// TestPublicStreamIgnoresHiddenActivity runs the public stream against the
// real profiles service: time in a hidden project, and agent time, must not
// produce a public event; credited time in a visible project must.
func TestPublicStreamIgnoresHiddenActivity(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	alice := seedtest.User(t, st, "alice", "UTC", true)
	start := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	seedtest.Span(t, st, alice, start, 10, seedtest.Minute{Project: "logbook"})
	seedtest.Span(t, st, alice, start.Add(10*time.Minute), 10, seedtest.Minute{Project: "secret"})

	hub := live.NewHub(live.DefaultLimits())
	profileSvc := profiles.NewService(st, hub)
	hidden := true
	if _, err := projects.NewService(st, profileSvc).Update(ctx, alice, "secret", projects.Patch{Hidden: &hidden}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	live.NewHandler(hub, profileSvc, httpapitest.StubSession, httpapitest.NoLimit,
		live.Config{PublicInterval: 20 * time.Millisecond}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	reqCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/api/v1/users/alice/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	events := make(chan string, 16)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				events <- name
			}
		}
	}()
	expect := func(want string, within time.Duration) {
		t.Helper()
		select {
		case got := <-events:
			if want == "" {
				t.Fatalf("unexpected public event %q", got)
			}
			if got != want {
				t.Fatalf("event = %q, want %q", got, want)
			}
		case <-time.After(within):
			if want != "" {
				t.Fatalf("no %q event within %v", want, within)
			}
		}
	}
	expect("ready", 5*time.Second)

	at := start.Add(30 * time.Minute)
	seedtest.Span(t, st, alice, at, 5, seedtest.Minute{Project: "secret"})
	hub.MinutesChanged(alice, at, at.Add(5*time.Minute))
	expect("", 300*time.Millisecond)

	at = at.Add(5 * time.Minute)
	seedtest.Span(t, st, alice, at, 5, seedtest.Minute{Project: "logbook", Label: "agent"})
	hub.MinutesChanged(alice, at, at.Add(5*time.Minute))
	expect("", 300*time.Millisecond)

	at = at.Add(5 * time.Minute)
	seedtest.Span(t, st, alice, at, 5, seedtest.Minute{Project: "logbook"})
	hub.MinutesChanged(alice, at, at.Add(5*time.Minute))
	expect("minutes", 5*time.Second)
}
