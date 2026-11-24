package programapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/stats"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

// testNow is the clock of every handler test: the fixtures' activity is in
// June 2026, so default ranges cover it and a range ending in June is closed.
var testNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// fakeAuth stands in for packages oauth and auth.
type fakeAuth struct {
	tokens map[string]Grant
	keys   map[string]uuid.UUID
}

func (f *fakeAuth) AccessToken(_ context.Context, token string) (Grant, error) {
	if g, ok := f.tokens[token]; ok {
		return g, nil
	}
	return Grant{}, ErrInvalidCredentials
}

func (f *fakeAuth) APIKey(_ context.Context, key string) (uuid.UUID, error) {
	if id, ok := f.keys[key]; ok {
		return id, nil
	}
	return uuid.Nil, ErrInvalidCredentials
}

// Two OAuth apps, so tests can show a token's app doesn't widen access.
var (
	appOne = uuid.MustParse("00000000-0000-4000-8000-0000000000a1")
	appTwo = uuid.MustParse("00000000-0000-4000-8000-0000000000a2")
)

// fixture is a database seeded with the users every handler test shares.
type fixture struct {
	st   *store.Store
	auth *fakeAuth
	mux  *http.ServeMux

	orpheus, pat, yellowy, unknowny, banned, numeric, nodevice uuid.UUID
	orpheusID, patID                                           int64
}

func day(h, m int) time.Time { return time.Date(2026, 6, 10, h, m, 0, 0, time.UTC) }

// newFixture seeds:
//
//   - orpheus (public, Hackatime 4242 green, a device): raw minutes on
//     logbook (30 min coding Go, 10 min "ai coding"), site (5 min browsing
//     Svelte), secret (hidden, 20 min), old (archived, 5 min); counted
//     minutes on logbook (20 min), site (2 × 45 s), secret (5 min) and
//     agent-only (3 min, no heartbeats); heartbeats behind them;
//   - pat (private, unlinked) with 15 raw and 10 counted minutes on diary;
//   - yellowy (yellow), unknowny (unknown), banned (red, public);
//   - "42" (a numeric username) and nodevice (raw time, never had a device).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t)
	f := &fixture{st: st}
	f.orpheus = seedtest.User(t, st, "orpheus", "UTC", true)
	f.pat = seedtest.User(t, st, "pat", "UTC", false)
	f.yellowy = seedtest.User(t, st, "yellowy", "UTC", true)
	f.unknowny = seedtest.User(t, st, "unknowny", "UTC", true)
	f.banned = seedtest.User(t, st, "banned", "UTC", true)
	f.numeric = seedtest.User(t, st, "42", "UTC", true)
	f.nodevice = seedtest.User(t, st, "nodevice", "UTC", true)
	f.orpheusID = seedtest.PublicID(t, st, f.orpheus)
	f.patID = seedtest.PublicID(t, st, f.pat)

	seedtest.HackatimeLink(t, st, f.orpheus, 4242, "orpheus-gh", "green")
	seedtest.HackatimeLink(t, st, f.yellowy, 4243, "", "yellow")
	seedtest.HackatimeLink(t, st, f.unknowny, 4244, "", "unknown")
	seedtest.HackatimeLink(t, st, f.banned, 4245, "", "red")

	// orpheus
	o := f.orpheus
	seedtest.Span(t, st, o, day(9, 0), 30, seedtest.Minute{Project: "logbook", Language: "Go"})
	seedtest.Span(t, st, o, day(9, 30), 10, seedtest.Minute{Project: "logbook", Language: "Go", Category: "ai coding"})
	seedtest.Span(t, st, o, day(10, 0), 5, seedtest.Minute{Project: "site", Language: "Svelte", Category: "browsing"})
	seedtest.Span(t, st, o, day(11, 0), 20, seedtest.Minute{Project: "secret", Language: "Rust"})
	seedtest.Span(t, st, o, day(12, 0), 5, seedtest.Minute{Project: "old", Language: "C"})
	seedtest.ProjectSettings(t, st, o, "secret", "", true, false)
	seedtest.ProjectSettings(t, st, o, "old", "", false, true)
	seedtest.ProjectSettings(t, st, o, "logbook", "https://github.com/orpheus/logbook", false, false)
	dev := seedtest.Device(t, st, o, "laptop")
	seedtest.CreditedSpan(t, st, o, dev, day(9, 0), 20, seedtest.Credited{Project: "logbook", Seconds: 60})
	seedtest.CreditedSpan(t, st, o, dev, day(10, 0), 2, seedtest.Credited{Project: "site", Seconds: 45})
	seedtest.CreditedSpan(t, st, o, dev, day(11, 0), 5, seedtest.Credited{Project: "secret", Seconds: 60})
	seedtest.CreditedSpan(t, st, o, dev, day(13, 0), 3, seedtest.Credited{Project: "agent-only", Seconds: 60})
	var hbs []seedtest.Heartbeat
	for i := range 40 { // 09:00 .. 09:39, one a minute
		cat := "coding"
		if i >= 30 {
			cat = "ai coding"
		}
		hbs = append(hbs, seedtest.Heartbeat{At: day(9, i), Project: "logbook", Category: cat})
	}
	hbs = append(hbs,
		seedtest.Heartbeat{At: day(10, 0), Project: "site", Language: "Svelte", Category: "browsing", Entity: "https://example.com"},
		seedtest.Heartbeat{At: day(10, 0).Add(30 * time.Second), Project: "site", Language: "Svelte", Category: "browsing", Entity: "https://example.com"},
		seedtest.Heartbeat{At: day(11, 0), Project: "secret", Language: "Rust", Entity: "secret.rs"},
		seedtest.Heartbeat{At: day(12, 0), Project: "old", Language: "C", Entity: "old.c"},
	)
	seedtest.Heartbeats(t, st, o, hbs...)

	// pat (private)
	seedtest.Span(t, st, f.pat, day(9, 0), 15, seedtest.Minute{Project: "diary", Language: "Markdown"})
	pdev := seedtest.Device(t, st, f.pat, "desktop")
	seedtest.CreditedSpan(t, st, f.pat, pdev, day(9, 0), 10, seedtest.Credited{Project: "diary", Seconds: 60})
	seedtest.Heartbeats(t, st, f.pat, seedtest.Heartbeat{At: day(9, 0), Project: "diary", Language: "Markdown"},
		seedtest.Heartbeat{At: day(9, 1), Project: "diary", Language: "Markdown"})

	seedtest.Span(t, st, f.banned, day(9, 0), 5, seedtest.Minute{Project: "banned-proj"})
	seedtest.Span(t, st, f.nodevice, day(9, 0), 7, seedtest.Minute{Project: "plain"})

	f.auth = &fakeAuth{
		tokens: map[string]Grant{
			"orpheus-profile":      {UserID: o, AppID: appOne, Scopes: []string{ScopeProfile}},
			"orpheus-read":         {UserID: o, AppID: appOne, Scopes: []string{ScopeRead}},
			"orpheus-profile-read": {UserID: o, AppID: appOne, Scopes: []string{ScopeProfile, ScopeRead}},
			"pat-profile":          {UserID: f.pat, AppID: appOne, Scopes: []string{ScopeProfile}},
			"pat-read-app2":        {UserID: f.pat, AppID: appTwo, Scopes: []string{ScopeProfile, ScopeRead}},
			"banned-read":          {UserID: f.banned, AppID: appOne, Scopes: []string{ScopeProfile, ScopeRead}},
		},
		keys: map[string]uuid.UUID{
			"11111111-1111-4111-8111-111111111111": o,
			"22222222-2222-4222-8222-222222222222": f.pat,
			"33333333-3333-4333-8333-333333333333": f.banned,
		},
	}
	f.mux = f.newMux(t, nil)
	return f
}

const (
	orpheusKey = "11111111-1111-4111-8111-111111111111"
	patKey     = "22222222-2222-4222-8222-222222222222"
	bannedKey  = "33333333-3333-4333-8333-333333333333"
)

// newMux wires a Handler to the fixture; limiter nil means a generous one.
func (f *fixture) newMux(t *testing.T, limiter *httpapi.RateLimiter) *http.ServeMux {
	t.Helper()
	svc := NewService(f.st, stats.NewService(f.st), func() time.Time { return testNow })
	mux := http.NewServeMux()
	NewHandler(svc, f.auth, Options{Limiter: limiter, ClientKey: httpapi.IPKey(false)}).Register(mux)
	return mux
}

// call describes one request.
type call struct {
	target string
	bearer string // Authorization: Bearer
	basic  string // Authorization: Basic base64(basic)
	remote string // RemoteAddr; default 192.0.2.1:1234
}

func (f *fixture) do(t *testing.T, c call) *httptest.ResponseRecorder {
	t.Helper()
	return doOn(t, f.mux, c)
}

func doOn(t *testing.T, mux http.Handler, c call) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, c.target, nil)
	r.RemoteAddr = "192.0.2.1:1234"
	if c.remote != "" {
		r.RemoteAddr = c.remote
	}
	switch {
	case c.bearer != "":
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	case c.basic != "":
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.basic)))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// decode unmarshals a response body into a generic map.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return m
}
