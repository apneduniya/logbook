//go:build demoseed

// The demo data behind the docs screenshots (scripts/docs-shots). It is
// made through the real code paths, not inserted rows: accounts, profiles,
// API keys, apps, reviewers, the OAuth grant and review decisions go
// through the real API (router.New, signed in with POST /auth/dev-login);
// ten days of work run through the agent's real crediting pipeline (the
// e2e rig: local event log, struggle engine, evidence sealing, heartbeat
// upload) against the server's real minutes, evidence and heartbeat
// endpoints; and submissions are made by the agent's `logbook submit` and
// re-checked by the server's River job. Everything is made up.
//
// It drops and recreates its own database (never "logbook", which the
// other tests use), then leaves it in place for a server to run against.
// From the repo root, with the dev Postgres up (make db-up):
//
//	go test -tags demoseed -run TestDemoSeed -count=1 -timeout 30m -v ./internal/e2e
//
// Settings (environment):
//
//	DEMO_DATABASE_URL    default postgres://logbook:logbook@127.0.0.1:54329/logbook_demo?sslmode=disable;
//	                     the database name must start with logbook_demo
//	DEMO_WEB_URL         the web UI origin review links point at, default http://localhost:18481
//	DEMO_ENCRYPTION_KEY  base64, 32 bytes; default is a fixed, public demo key.
//	                     The server run against the database must use the same one,
//	                     or it can't read the stored review links.
//
// The activity is laid out on the ten UTC days ending today, so run it on
// the day the screenshots are taken. Today's work ends 40 minutes before
// the seed runs, which needs it to run after 05:00 UTC.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for migrations
	"github.com/riverqueue/river"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/submit"
	"github.com/apneduniya/logbook/internal/agent/upload"
	"github.com/apneduniya/logbook/internal/server/admin"
	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/jobs"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/programapi"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/router"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/stats"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/submissions"
	"github.com/apneduniya/logbook/migrations"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	demoDefaultURL = "postgres://logbook:logbook@127.0.0.1:54329/logbook_demo?sslmode=disable"
	demoDefaultWeb = "http://localhost:18481"
	// demoKeyText is the default encryption key: public, for demo data only.
	demoKeyText = "logbook-demo-key-not-a-secret!!!"
	// demoProgram is the program's slug.
	demoProgram   = "example-program"
	demoRedirect  = "https://program.example/logbook/callback"
	demoUserAgent = "logbook-agent/demo"
)

func demoEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func TestDemoSeed(t *testing.T) {
	web := strings.TrimRight(demoEnv("DEMO_WEB_URL", demoDefaultWeb), "/")
	key, err := base64.StdEncoding.DecodeString(demoEnv("DEMO_ENCRYPTION_KEY",
		base64.StdEncoding.EncodeToString([]byte(demoKeyText))))
	if err != nil || len(key) != 32 {
		t.Fatalf("DEMO_ENCRYPTION_KEY must be base64 of 32 bytes")
	}
	st := resetDemoDB(t)
	api := newDemoAPI(t, st, key, web)

	// Accounts, as the web UI makes them: dev sign-in, then the profile.
	orpheus := api.signIn("orpheus")
	orpheus.call(http.MethodPatch, "/api/v1/me/profile",
		map[string]any{"display_name": "Orpheus", "timezone": "UTC", "profile_public": true}, http.StatusOK, nil)
	sam := api.signIn("reviewer-sam")
	sam.call(http.MethodPatch, "/api/v1/me/profile", map[string]any{"display_name": "Sam", "timezone": "UTC"}, http.StatusOK, nil)
	alex := api.signIn("reviewer-alex")
	alex.call(http.MethodPatch, "/api/v1/me/profile", map[string]any{"display_name": "Alex", "timezone": "UTC"}, http.StatusOK, nil)
	// nova has just signed up: no device, no history (the set-up home).
	nova := api.signIn("nova")
	nova.call(http.MethodPatch, "/api/v1/me/profile", map[string]any{"display_name": "Nova", "timezone": "UTC"}, http.StatusOK, nil)

	var apiKey struct {
		Key string `json:"key"`
	}
	orpheus.call(http.MethodPost, "/api/v1/api_keys", map[string]string{"name": "laptop"}, http.StatusCreated, &apiKey)

	// Example Program: Sam's app, taking submissions, with Alex as reviewer.
	var app struct {
		ID           string `json:"id"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	sam.call(http.MethodPost, "/api/v1/me/apps", map[string]any{
		"name": "Example Program", "slug": demoProgram, "redirect_uris": []string{demoRedirect},
		"homepage_url": "https://program.example", "public": true, "confidential": true,
	}, http.StatusCreated, &app)
	sam.call(http.MethodPost, "/api/v1/me/apps/"+app.ID+"/members", map[string]string{"username": "reviewer-alex"}, http.StatusCreated, nil)

	// Orpheus lets Example Program read their profile and stats, and the
	// program redeems the code.
	code := orpheus.authorize(app.ClientID, "profile read")
	api.redeem(app.ClientID, app.ClientSecret, code)

	// Ten days of work on the laptop, through the agent's pipeline.
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	dev := newDemoDevice(t, st, orpheus.id, today.AddDate(0, 0, -9).Add(14*time.Hour))
	for i, d := range demoDays() {
		day := today.AddDate(0, 0, i-9)
		start := day.Add(d.start)
		last := i == 9
		if last {
			// Today's work ends 40 minutes ago, so all of it is final.
			start = now.Truncate(time.Minute).Add(-40*time.Minute - time.Duration(d.length())*time.Minute)
			if start.Before(day.Add(time.Hour)) {
				t.Fatalf("run the seed after 05:00 UTC: today's work would start at %s", start.Format(time.DateTime))
			}
		}
		dev.work(start, d.segs)
		dev.flushHeartbeats()
		if !last {
			dev.sealDay(day.Format(time.DateOnly))
		} else {
			// Let today's minutes become final (30 minutes old) without
			// running the clock past now.
			end := now.Truncate(time.Minute).Add(-time.Minute)
			dev.runEvery(dev.clock().Truncate(time.Minute).Add(time.Minute), end, nil)
		}
	}
	dev.flushHeartbeats()

	// Submissions, made by `logbook submit` on the laptop and re-checked
	// by the server's job: tinyfs (approved by Alex), hackclub-bot and
	// pixel-garden (waiting for a decision).
	d := func(n int) string { return today.AddDate(0, 0, n).Format(time.DateOnly) }
	tinyfs := api.submit(dev, apiKey.Key, "tinyfs", d(-9), d(-5))
	alex.call(http.MethodPost, "/api/v1/review/"+tinyfs+"/decision",
		map[string]any{"value": "approved"}, http.StatusOK, nil)
	bot := api.submit(dev, apiKey.Key, "hackclub-bot", d(-7), d(-3))
	garden := api.submit(dev, apiKey.Key, "pixel-garden", d(-4), d(-1))
	for _, tok := range []string{tinyfs, bot, garden} {
		api.wantVerified(tok)
	}
	t.Logf("users: orpheus (data), reviewer-sam (owns Example Program), reviewer-alex (reviewer), nova (no device)")
	t.Logf("review pages: %s/r/%s (pixel-garden), %s/r/%s (hackclub-bot), %s/r/%s (tinyfs, approved)",
		web, garden, web, bot, web, tinyfs)
}

// resetDemoDB drops and recreates the demo database, migrates it and opens
// it.
func resetDemoDB(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	raw := demoEnv("DEMO_DATABASE_URL", demoDefaultURL)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("DEMO_DATABASE_URL: %v", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !regexp.MustCompile(`^logbook_demo[a-z0-9_]*$`).MatchString(name) {
		t.Fatalf("the demo database's name must start with logbook_demo, got %q", name)
	}
	adminURL := *u
	adminURL.Path = "/postgres"
	conn, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, q := range []string{"DROP DATABASE IF EXISTS " + name + " WITH (FORCE)", "CREATE DATABASE " + name} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sqlDB, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := jobs.Migrate(ctx, st.Pool); err != nil {
		t.Fatal(err)
	}
	return st
}

// demoAPI is the real API (router.New, wired as cmd/logbook-server wires
// it) on the demo database, with the submission re-check job running.
type demoAPI struct {
	t      *testing.T
	url    string
	web    string
	events <-chan *river.Event
}

func newDemoAPI(t *testing.T, st *store.Store, key []byte, web string) *demoAPI {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	authSvc, err := auth.New(st, auth.Options{
		BaseURL: "http://127.0.0.1:8080", WebURL: web, SecretKey: key, InsecureCookies: true, DevLogin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	htClient, err := hackatime.NewClient(hackatime.ClientConfig{RedirectURL: "http://127.0.0.1:8080/auth/hackatime/callback"})
	if err != nil {
		t.Fatal(err)
	}
	hub := live.NewHub(live.DefaultLimits())
	t.Cleanup(hub.Close)
	statsSvc := stats.NewService(st)
	profileSvc := profiles.NewService(st, hub)
	oauthSvc := oauth.NewService(st, oauth.Options{WebURL: web})
	ledgerSvc := ledger.NewService(st)
	slots := submissions.NewDecodeSlots(0)
	checker := submissions.NewChecker(st, ledgerSvc, submissions.CheckerOptions{DecodeSlots: slots, Logger: logger})
	workers := river.NewWorkers()
	if err := submissions.AddWorkers(workers, checker); err != nil {
		t.Fatal(err)
	}
	jc, err := jobs.New(st.Pool, jobs.Options{Workers: workers, Queues: map[string]int{submissions.Queue: 1}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := jc.River().Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(cancel)
	if err := jc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := jc.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	h := router.New(router.Services{
		Ping:        st.Pool.Ping,
		Auth:        authSvc,
		OAuth:       oauthSvc,
		Stats:       statsSvc,
		Profiles:    profileSvc,
		Projects:    projects.NewService(st, profileSvc),
		Hub:         hub,
		Ingest:      ingest.New(st, live.Fanout{statsSvc, hub}),
		Devices:     devices.NewService(st),
		Credited:    credited.NewService(st, hub),
		Ledger:      ledgerSvc,
		Hackatime:   hackatime.NewService(st, box, htClient, jc.River(), hackatime.ServiceOptions{Logger: logger}),
		ProgramAPI:  programapi.NewService(st, statsSvc, nil),
		Submissions: submissions.NewService(st, box, submissions.Options{ReviewBaseURL: web, Enqueuer: jc.River(), Members: oauthSvc, DecodeSlots: slots, Logger: logger}),
		Admin:       admin.NewService(st, checker, nil),
		AdminKeys:   admin.NewKeys(st, nil),
	}, router.Options{WebOrigin: web})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &demoAPI{t: t, url: srv.URL, web: web, events: events}
}

// demoBrowser is a signed-in web UI session.
type demoBrowser struct {
	api    *demoAPI
	client *http.Client
	id     uuid.UUID
	csrf   string
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// signIn signs username in through POST /auth/dev-login, creating the
// account the first time.
func (a *demoAPI) signIn(username string) *demoBrowser {
	a.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		a.t.Fatal(err)
	}
	b := &demoBrowser{api: a, client: &http.Client{Jar: jar, Timeout: time.Minute, CheckRedirect: noRedirects}}
	var out struct {
		User struct {
			ID uuid.UUID `json:"id"`
		} `json:"user"`
		CSRF string `json:"csrf_token"`
	}
	b.call(http.MethodPost, "/auth/dev-login", map[string]string{"username": username}, http.StatusOK, &out)
	b.id, b.csrf = out.User.ID, out.CSRF
	return b
}

// call sends a JSON request with the session cookie (and the CSRF header on
// writes), fails unless the answer is want, and decodes it into out.
func (b *demoBrowser) call(method, path string, body any, want int, out any) http.Header {
	b.api.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.api.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, b.api.url+path, rd)
	if err != nil {
		b.api.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && b.csrf != "" {
		req.Header.Set(auth.CSRFHeader, b.csrf)
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.api.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		b.api.t.Fatal(err)
	}
	if res.StatusCode != want {
		b.api.t.Fatalf("%s %s = %d %s, want %d", method, path, res.StatusCode, raw, want)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			b.api.t.Fatalf("%s %s: decoding %s: %v", method, path, raw, err)
		}
	}
	return res.Header
}

// authorize runs GET /oauth/authorize and the consent page's calls,
// approving, and returns the code.
func (b *demoBrowser) authorize(clientID, scope string) string {
	t := b.api.t
	t.Helper()
	q := url.Values{"client_id": {clientID}, "redirect_uri": {demoRedirect}, "response_type": {"code"},
		"scope": {scope}, "state": {"demo"}}
	h := b.call(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, http.StatusFound, nil)
	loc, err := url.Parse(h.Get("Location"))
	if err != nil || loc.Query().Get("request") == "" {
		t.Fatalf("authorize sent the browser to %q", h.Get("Location"))
	}
	path := "/api/v1/oauth/requests/" + loc.Query().Get("request")
	b.call(http.MethodGet, path, nil, http.StatusOK, nil)
	var out struct {
		Redirect string `json:"redirect"`
	}
	b.call(http.MethodPost, path, map[string]bool{"approve": true}, http.StatusOK, &out)
	back, err := url.Parse(out.Redirect)
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("consent redirect = %q", out.Redirect)
	}
	return back.Query().Get("code")
}

// redeem exchanges code at POST /oauth/token as the program's server does.
func (a *demoAPI) redeem(clientID, secret, code string) {
	a.t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {demoRedirect}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.url+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body) // only for the message
		a.t.Fatalf("token = %d %s", res.StatusCode, raw)
	}
}

// submit runs `logbook submit --yes` on dev for project over [from, to] to
// Example Program, waits for the re-check and returns the review token.
func (a *demoAPI) submit(dev *demoDevice, key, project, from, to string) string {
	a.t.Helper()
	var out bytes.Buffer
	env := submit.Env{
		Server: serverapi.New(&http.Client{Timeout: time.Minute}, demoUserAgent),
		Target: serverapi.Target{BaseURL: a.url, APIKey: key}, DeviceID: dev.device.String(),
		Prove: func(ctx context.Context, keys []replay.Key) (crediting.Proofs, error) {
			return crediting.BuildProofs(ctx, dev.db, dev.device.String(), keys)
		},
		In: strings.NewReader(""), Out: &out, Interactive: false, Now: time.Now,
	}
	req := submit.Request{Project: project, From: from, To: to, Program: demoProgram, Yes: true}
	if err := submit.Run(context.Background(), req, env); err != nil {
		a.t.Fatalf("submit %s: %v\n%s", project, err, out.String())
	}
	link := reviewRe.FindStringSubmatch(out.String())
	if link == nil {
		a.t.Fatalf("submit %s printed no review link:\n%s", project, out.String())
	}
	token, ok := strings.CutPrefix(link[1], a.web+"/r/")
	if !ok {
		a.t.Fatalf("review link %s", link[1])
	}
	a.work()
	return token
}

// work waits for one re-check job to finish.
func (a *demoAPI) work() {
	a.t.Helper()
	deadline := time.After(5 * time.Minute)
	for {
		select {
		case ev := <-a.events:
			if ev.Job.Kind != (submissions.VerifyArgs{}).Kind() {
				continue
			}
			if ev.Kind != river.EventKindJobCompleted {
				a.t.Fatalf("re-check job %d: %s %+v", ev.Job.ID, ev.Kind, ev.Job.Errors)
			}
			return
		case <-deadline:
			a.t.Fatal("the re-check didn't finish in 5 minutes")
		}
	}
}

// wantVerified checks, signed out, that the review page's packet is ready
// with every minute verified.
func (a *demoAPI) wantVerified(token string) {
	a.t.Helper()
	b := &demoBrowser{api: a, client: &http.Client{Timeout: time.Minute, CheckRedirect: noRedirects}}
	var got reviewed
	b.call(http.MethodGet, "/api/v1/review/"+token, nil, http.StatusOK, &got)
	if got.Submission.Status != "ready" || got.Packet == nil {
		a.t.Fatalf("review %s = %+v", got.Submission.Project, got.Submission)
	}
	if m := got.Packet.Minutes; m.Unbacked != 0 || m.Mismatch != 0 || m.Verified == 0 {
		a.t.Fatalf("review %s minutes = %+v, want all verified", got.Submission.Project, m)
	}
}

// demoDevice is a rig whose device is registered through the devices
// service, whose server side also takes heartbeats (ingest), and whose
// heartbeat queue is sent by the agent's upload worker.
type demoDevice struct {
	*rig
	hb *upload.Worker
}

// Where each project lives on the laptop, its language, files and editor.
var demoProjects = map[string]struct {
	path, language, ua string
	files              []string
}{
	"pixel-garden": {"/home/orpheus/code/pixel-garden", "TypeScript",
		"wakatime/v1.130.0 (darwin-25.1.0-arm64) go1.25.0 vscode/1.106.1 vscode-wakatime/25.4.0",
		[]string{"src/lib/garden.ts", "src/lib/sprites.ts", "src/routes/+page.svelte", "src/lib/soil.ts"}},
	"tinyfs": {"/home/orpheus/code/tinyfs", "Rust",
		"wakatime/v1.130.0 (darwin-25.1.0-arm64) go1.25.0 neovim/0.11.4 vim-wakatime/11.3.0",
		[]string{"src/inode.rs", "src/block.rs", "src/journal.rs", "tests/mount.rs"}},
	"hackclub-bot": {"/home/orpheus/code/hackclub-bot", "Python",
		"wakatime/v1.130.0 (darwin-25.1.0-arm64) go1.25.0 vscode/1.106.1 vscode-wakatime/25.4.0",
		[]string{"bot/commands.py", "bot/events.py", "bot/db.py", "tests/test_commands.py"}},
}

func newDemoDevice(t *testing.T, st *store.Store, user uuid.UUID, start time.Time) *demoDevice {
	t.Helper()
	r := &rig{t: t, st: st, user: user, now: start, computed: map[time.Time][]struggle.Minute{},
		bundle: map[string][]evidence.Row{}, registered: start.UTC().Truncate(24 * time.Hour)}
	var err error
	r.key, err = devicekey.Create(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := devices.NewService(st, devices.WithClock(r.clock)).Register(context.Background(), user,
		devices.Registration{Name: "laptop", OS: "darwin", PublicKey: r.key.Public()})
	if err != nil {
		t.Fatal(err)
	}
	r.device = reg.ID

	// As in newRig: fresh handlers (and rate limiters) for each reading of
	// the rig's clock, the server's clock following the rig's.
	var (
		mux   *http.ServeMux
		muxAt time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if mux == nil || !muxAt.Equal(r.now) {
			mux, muxAt = http.NewServeMux(), r.now
			credited.NewHandler(credited.NewService(r.st, nil, credited.WithClock(r.serverClock)), httpapitest.StubSession).Register(mux)
			ledger.NewHandler(ledger.NewService(r.st, ledger.WithClock(r.serverClock)), httpapitest.StubSession).Register(mux)
			ingest.NewHandler(ingest.New(r.st, nil, ingest.WithClock(r.serverClock)), httpapitest.StubSession).Register(mux)
		}
		handler := mux
		r.mu.Unlock()
		req.Header.Set(httpapitest.UserHeader, r.user.String())
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.client = serverapi.New(srv.Client(), demoUserAgent)
	r.target = serverapi.Target{BaseURL: srv.URL, APIKey: "key"}
	r.dbPath = filepath.Join(t.TempDir(), "agent.db")
	r.db = r.openDB(r.dbPath)
	r.identity = &identity{dev: devicekey.Device{ID: r.device.String(), RegisteredAt: r.registered, Key: r.key}}
	for name, p := range demoProjects {
		r.repos = append(r.repos, struggle.Repo{Path: p.path, Project: name})
	}
	r.opts = crediting.Options{
		Store: r.db, Server: r.client, Identity: r.identity,
		Target: func() crediting.Target { return crediting.Target{Target: r.target} },
		Repos:  r, Compute: r.compute, TZ: time.UTC, Now: r.clock,
		Jitter: func() float64 { return 0 }, Settle: -1, Salts: zeroSalts{},
	}
	r.runner = crediting.New(r.opts)
	hb := upload.New(upload.Options{
		Name: "logbook", Queue: localdb.HeartbeatQueue, Store: r.db, Client: srv.Client(), UserAgent: demoUserAgent, Now: r.clock,
		Target: func() upload.Target {
			return upload.Target{URL: srv.URL + "/api/v1/users/current/heartbeats.bulk", APIKey: "key",
				Headers: map[string]string{"X-Logbook-Source": "agent"}}
		},
	})
	return &demoDevice{rig: r, hb: hb}
}

// flushHeartbeats sends the queued heartbeats now and fails unless all
// were taken.
func (d *demoDevice) flushHeartbeats() {
	d.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = d.hb.Run(ctx) // always nil; with ctx done it makes one bounded attempt
	n, err := d.db.Pending(context.Background(), localdb.HeartbeatQueue)
	if err != nil {
		d.t.Fatal(err)
	}
	if n != 0 {
		d.t.Fatalf("%d heartbeats not sent: %+v", n, d.hb.State())
	}
}

// A segment is a stretch of minutes of one kind of work on a project.
type segKind int

const (
	hands  segKind = iota // editing by hand
	withAI                // prompting an agent and watching it, editing now and then
	away                  // the agent working alone after a prompt
	stamp                 // approving the agent with "yes" turn after turn
	loop                  // sending the same long prompt again and again
	gap                   // nothing at all
)

type seg struct {
	project string
	kind    segKind
	minutes int
	commit  bool // a commit at the end of the segment
}

type demoDay struct {
	start time.Duration // after midnight UTC (today's is moved to end before now)
	segs  []seg
}

func (d demoDay) length() int {
	n := 0
	for _, s := range d.segs {
		n += s.minutes
	}
	return n + 5
}

// demoDays is the work of the ten days ending today, oldest first.
func demoDays() []demoDay {
	const pg, fs, bot = "pixel-garden", "tinyfs", "hackclub-bot"
	h := time.Hour
	return []demoDay{
		{16 * h, []seg{{fs, hands, 30, false}, {fs, withAI, 10, true}}},
		{15*h + 30*time.Minute, []seg{{fs, hands, 40, false}, {fs, away, 8, false}, {fs, hands, 12, true}}},
		{17 * h, []seg{{bot, hands, 25, false}, {bot, withAI, 12, true}}},
		{14 * h, []seg{{bot, hands, 30, false}, {bot, withAI, 10, false}, {bot, away, 10, true}, {fs, gap, 20, false}, {fs, hands, 15, false}}},
		{16 * h, []seg{{fs, hands, 35, false}, {fs, withAI, 12, false}, {fs, loop, 14, false}, {fs, hands, 8, true}}},
		{15 * h, []seg{{pg, hands, 30, false}, {pg, withAI, 15, false}, {pg, hands, 15, true}}},
		{16 * h, []seg{{pg, hands, 20, false}, {pg, withAI, 20, false}, {pg, stamp, 7, false}, {pg, hands, 10, true}, {bot, gap, 30, false}, {bot, hands, 20, true}}},
		{18 * h, []seg{{pg, hands, 25, false}, {pg, withAI, 15, false}, {pg, away, 8, true}}},
		{15 * h, []seg{{pg, hands, 30, false}, {pg, withAI, 25, false}, {pg, away, 10, false}, {pg, hands, 10, true}}},
		{0, []seg{{pg, hands, 20, false}, {pg, withAI, 15, false}, {pg, away, 6, false}, {pg, hands, 15, true}, {fs, hands, 10, false}}},
	}
}

// work runs the agent once a minute from start through segs, logging each
// minute's events before the run that follows it, then five quiet minutes.
func (d *demoDevice) work(start time.Time, segs []seg) {
	d.t.Helper()
	at := start
	for si, s := range segs {
		w := &segWriter{d: d, s: s, id: fmt.Sprintf("%s-%d", start.Format(time.DateOnly), si)}
		for i := range s.minutes {
			d.set(at.Add(time.Minute + 5*time.Second))
			w.minute(at, i)
			d.run()
			at = at.Add(time.Minute)
		}
	}
	for range 5 {
		d.set(at.Add(time.Minute + 5*time.Second))
		d.run()
		at = at.Add(time.Minute)
	}
}

// segWriter logs one segment's events, minute by minute.
type segWriter struct {
	d  *demoDevice
	s  seg
	id string
}

func (w *segWriter) minute(at time.Time, i int) {
	d, s := w.d, w.s
	last := i == s.minutes-1
	switch s.kind {
	case hands:
		d.present(at, at.Add(time.Minute))
		w.heartbeat(at.Add(20*time.Second), i, true)
		w.heartbeat(at.Add(45*time.Second), i, false)
	case withAI:
		d.present(at, at.Add(time.Minute))
		// A new turn every 4 minutes: prompt, tool calls, turn end.
		if i%4 == 0 {
			w.ai(at.Add(5*time.Second), wire.AIPromptSubmit, fmt.Sprintf("%s-%d", w.id, i), 90+7*i)
		}
		w.ai(at.Add(25*time.Second), wire.AIToolStart, "", 0)
		w.ai(at.Add(35*time.Second), wire.AIToolEnd, "", 0)
		if i%4 == 3 || last {
			w.ai(at.Add(50*time.Second), wire.AITurnEnd, "", 0)
		}
		if i%2 == 1 {
			w.heartbeat(at.Add(40*time.Second), i, true)
		}
	case away:
		if i == 0 {
			d.present(at, at.Add(10*time.Second))
			w.ai(at.Add(5*time.Second), wire.AIPromptSubmit, w.id, 240)
		}
		w.ai(at.Add(30*time.Second), wire.AIToolStart, "", 0)
		w.ai(at.Add(45*time.Second), wire.AIToolEnd, "", 0)
		if last {
			w.ai(at.Add(55*time.Second), wire.AITurnEnd, "", 0)
		}
	case stamp:
		// Three short turns a minute, each approved with "yes" 10 s after
		// the agent stops.
		d.present(at, at.Add(time.Minute))
		for k := range 3 {
			off := time.Duration(k) * 20 * time.Second
			w.ai(at.Add(off), wire.AIPromptSubmit, "yes", 3)
			w.ai(at.Add(off+3*time.Second), wire.AIToolStart, "", 0)
			w.ai(at.Add(off+10*time.Second), wire.AITurnEnd, "", 0)
		}
	case loop:
		// The same long prompt every 3 minutes, nothing edited by hand.
		d.present(at, at.Add(time.Minute))
		if i%3 == 0 {
			w.ai(at.Add(5*time.Second), wire.AIPromptSubmit, w.id, 120)
		}
		w.ai(at.Add(30*time.Second), wire.AIToolStart, "", 0)
		if i%3 == 2 || last {
			w.ai(at.Add(55*time.Second), wire.AITurnEnd, "", 0)
		}
	case gap:
	}
	if last && s.commit {
		sum := sha256.Sum256([]byte(w.id + "-commit"))
		d.store(wire.LocalEvent{Type: "git", Git: &wire.GitEvent{Time: at.Add(58 * time.Second), Project: s.project,
			Kind: "commit", CommitHash: hex.EncodeToString(sum[:20]), FilesChanged: 2 + len(w.id)%4,
			Insertions: 30 + 7*s.minutes, Deletions: 4 + s.minutes/3}})
	}
}

// heartbeat queues one editor heartbeat, as the editor's plugin sends it
// through the agent: a save when write, otherwise the cursor moving.
func (w *segWriter) heartbeat(t time.Time, i int, write bool) {
	p := demoProjects[w.s.project]
	lines, line, col := 120+i, 10+3*i, 4+i%20
	w.d.enqueue(wire.Heartbeat{
		Entity: p.path + "/" + p.files[(i/6)%len(p.files)], Type: "file", Category: "coding",
		Project: w.s.project, Language: p.language, IsWrite: write, Lines: &lines, Lineno: &line, Cursorpos: &col,
		UserAgent: p.ua, OperatingSystem: "darwin", Time: float64(t.UnixMilli()) / 1000,
	})
}

// ai logs one Claude Code hook event in the segment's project; a prompt
// carries its length and a keyed hash of its text (here, of key).
func (w *segWriter) ai(t time.Time, kind wire.AIEventKind, key string, chars int) {
	p := demoProjects[w.s.project]
	e := wire.AIEvent{Time: t, Harness: wire.HarnessClaudeCode, Kind: kind, SessionID: "demo-" + w.id,
		Project: w.s.project, Cwd: p.path}
	if kind == wire.AIPromptSubmit {
		sum := sha256.Sum256([]byte(key))
		e.PromptChars, e.PromptHash = chars, hex.EncodeToString(sum[:])
	}
	w.d.store(wire.LocalEvent{Type: "ai", AI: &e})
}
