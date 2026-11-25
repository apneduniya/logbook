package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/apneduniya/logbook/internal/agent/audit"
	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/submit"
	"github.com/apneduniya/logbook/internal/server/admin"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/jobs"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/submissions"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Submissions end to end (ADR 0020): the agent's real `logbook submit`
// against the server's real submission handlers, re-check job (worked
// through River) and Postgres, then the review link, a program's decision
// and `logbook verify` on the bundle exported from what the server stored.

const reviewWeb = "https://web.test"

// subServer is the server's submission side on the rigs' store: the
// agent's calls (an API key, standing in as the rigs' user) go through
// its HTTP server; browser calls go to the mux as any user, or none.
type subServer struct {
	t       *testing.T
	r       *rig
	checker *submissions.Checker
	oauth   *oauth.Service
	river   *river.Client[pgx.Tx]
	// events reports each finished or failed job of the started client.
	events <-chan *river.Event
	// start starts the River client; work calls it the first time.
	start   func()
	started bool
	mux     *http.ServeMux
	client  *serverapi.Client
	target  serverapi.Target
	// admin is the admin API (ADR 0022) on the same store and checker;
	// adminKeyFor returns a key of the admin "auditor", made on first use.
	admin       *http.ServeMux
	adminKey    string
	adminKeyFor func() string
}

// serverNow is the submission server's clock: the morning after the last
// submitted day.
var serverNow = time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)

func newSubServer(t *testing.T, r *rig) *subServer {
	t.Helper()
	clock := func() time.Time { return serverNow }
	s := &subServer{t: t, r: r, mux: http.NewServeMux(), admin: http.NewServeMux()}
	s.oauth = oauth.NewService(r.st, oauth.Options{WebURL: reviewWeb, Now: clock})
	slots := submissions.NewDecodeSlots(0)
	s.checker = submissions.NewChecker(r.st, ledger.NewService(r.st, ledger.WithClock(clock)),
		submissions.CheckerOptions{DecodeSlots: slots, Now: clock})
	workers := river.NewWorkers()
	if err := submissions.AddWorkers(workers, s.checker); err != nil {
		t.Fatal(err)
	}
	jc, err := jobs.New(r.st.Pool, jobs.Options{Workers: workers, Queues: map[string]int{submissions.Queue: 1},
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	s.river = jc.River()
	events, cancel := s.river.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(cancel)
	s.events = events
	s.start = func() {
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
	}
	box, err := secretbox.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := submissions.NewService(r.st, box, submissions.Options{
		ReviewBaseURL: reviewWeb, Enqueuer: s.river, Members: s.oauth, DecodeSlots: slots, Now: clock,
	})
	submissions.NewHandler(svc, submissions.HandlerOptions{
		RequireSession: httpapitest.StubSession, RequireAPIKey: httpapitest.StubSession,
		RequireSubmissionsScope: s.oauth.RequireScope(oauth.ScopeSubmissions),
		ClientKey:               func(*http.Request) string { return "client" },
	}).Register(s.mux)
	keys := admin.NewKeys(r.st, clock)
	admin.NewHandler(admin.NewService(r.st, s.checker, clock), keys, admin.HandlerOptions{}).Register(s.admin)
	s.adminKeyFor = func() string {
		if s.adminKey == "" {
			seedtest.User(t, r.st, "auditor", "UTC", false)
			if _, err := keys.Grant(context.Background(), "auditor"); err != nil {
				t.Fatal(err)
			}
			k, err := keys.CreateKey(context.Background(), "auditor", "e2e")
			if err != nil {
				t.Fatal(err)
			}
			s.adminKey = k.Key
		}
		return s.adminKey
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set(httpapitest.UserHeader, r.user.String())
		s.mux.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	s.client = serverapi.New(srv.Client(), "logbook-agent/e2e")
	s.target = serverapi.Target{BaseURL: srv.URL, APIKey: "key"}
	return s
}

// call makes a browser request as user (uuid.Nil: signed out).
func (s *subServer) call(method, target string, user uuid.UUID, body any) *httptest.ResponseRecorder {
	s.t.Helper()
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		raw = string(b)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httpapitest.Request(s.t, method, target, user, raw))
	return rec
}

// expect is call, failing unless the answer is want; it decodes the body
// into out when out isn't nil.
func (s *subServer) expect(method, target string, user uuid.UUID, body any, want int, out any) {
	s.t.Helper()
	rec := s.call(method, target, user, body)
	if rec.Code != want {
		s.t.Fatalf("%s %s = %d %s, want %d", method, target, rec.Code, rec.Body, want)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			s.t.Fatalf("%s %s: decoding %s: %v", method, target, rec.Body, err)
		}
	}
}

// work waits until the server's River client (started on the first call)
// has run n more re-check jobs from the submissions queue to completion,
// and checks that no other re-check job is left. A job that fails fails
// the test.
func (s *subServer) work(n int) {
	s.t.Helper()
	if !s.started {
		s.start()
		s.started = true
	}
	deadline := time.After(5 * time.Minute)
	for done := 0; done < n; {
		select {
		case ev := <-s.events:
			if ev.Job.Kind != (submissions.VerifyArgs{}).Kind() {
				continue
			}
			if ev.Kind != river.EventKindJobCompleted || ev.Job.Queue != submissions.Queue {
				s.t.Fatalf("re-check job %d on %q: %s %+v", ev.Job.ID, ev.Job.Queue, ev.Kind, ev.Job.Errors)
			}
			done++
		case <-deadline:
			s.t.Fatalf("%d of %d re-check jobs ran in 5 minutes", done, n)
		}
	}
	if left := s.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state <> 'completed'`,
		(submissions.VerifyArgs{}).Kind()); left != 0 {
		s.t.Fatalf("%d re-check jobs left", left)
	}
}

// count runs a count query.
func (s *subServer) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.r.st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// status is a submission's status.
func (s *subServer) status(id string) string {
	s.t.Helper()
	var st string
	if err := s.r.st.Pool.QueryRow(context.Background(), `SELECT status FROM submissions WHERE id = $1`, id).Scan(&st); err != nil {
		s.t.Fatal(err)
	}
	return st
}

// results counts a submission's frozen rows of device by "result reason".
func (s *subServer) results(id string, device uuid.UUID) map[string]int {
	s.t.Helper()
	rows, err := s.r.st.Pool.Query(context.Background(),
		`SELECT coalesce(result, 'null'), reason FROM submission_minutes WHERE submission_id = $1 AND device_id = $2`, id, device)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var res, reason string
		if err := rows.Scan(&res, &reason); err != nil {
			s.t.Fatal(err)
		}
		out[strings.TrimSpace(res+" "+reason)]++
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// reviewed is GET /api/v1/review/{token}'s answer.
type reviewed struct {
	Submission struct {
		ID      string `json:"id"`
		Project string `json:"project"`
		Status  string `json:"status"`
	} `json:"submission"`
	Packet   *submission.Packet `json:"packet"`
	Decision *struct {
		Value           string `json:"value"`
		ApprovedSeconds *int64 `json:"approved_seconds"`
		Reason          string `json:"reason"`
		By              string `json:"by"`
	} `json:"decision"`
}

// review reads the review link signed out and checks its headers.
func (s *subServer) review(token string) reviewed {
	s.t.Helper()
	rec := s.call(http.MethodGet, "/api/v1/review/"+token, uuid.Nil, nil)
	if rec.Code != http.StatusOK {
		s.t.Fatalf("review = %d %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	if h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Robots-Tag") != "noindex" {
		s.t.Fatalf("review headers = %v", h)
	}
	var out reviewed
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// submitEnv is `logbook submit`'s environment on rig r, talking to s.
// Stdin isn't a terminal, so only --yes or --preview go ahead.
func (s *subServer) submitEnv(r *rig, out *bytes.Buffer) submit.Env {
	return submit.Env{
		Server: s.client, Target: s.target, DeviceID: r.device.String(),
		Prove: func(ctx context.Context, keys []replay.Key) (crediting.Proofs, error) {
			return crediting.BuildProofs(ctx, r.db, r.device.String(), keys)
		},
		In: strings.NewReader(""), Out: out, Interactive: false, Now: r.clock,
	}
}

// The three submitted days, each from 10:00 UTC.
var (
	day1 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	day2 = day1.AddDate(0, 0, 1)
	day3 = day1.AddDate(0, 0, 2)
)

const (
	subFrom = "2026-11-20"
	subTo   = "2026-11-22"
	// promptHash is the keyed hash ai() gives every prompt; it must never
	// reach the packet.
	promptHash = "abababababababababababababababababababababababababababababababab"
)

// workThreeDays has the laptop work on app for three days, with lib
// interleaved (busy), an AI agent working alone after the first session,
// /src/app mapped to lib for ten minutes of the second day, and a commit on
// the third; each day is sealed and checked. The desktop then works on app
// for fifteen minutes of the third day, eleven of them alongside the
// laptop, and seals it.
func workThreeDays(t *testing.T) (laptop, desktop *rig) {
	t.Helper()
	a := newRig(t, day1.Add(-time.Minute).Add(5*time.Second))
	both := []struggle.Repo{{Path: "/src/app", Project: "app"}, {Path: "/src/lib", Project: "lib"}}
	a.with(func(r *rig) { r.repos = both })
	a.runEvery(day1, day1.Add(40*time.Minute), func(now time.Time) {
		a.busy(now)
		if now.Truncate(time.Minute).Equal(day1.Add(6 * time.Minute)) {
			a.store(ai(now.Add(-40*time.Second), wire.AIPromptSubmit))
		}
	})
	a.runEvery(day1.Add(40*time.Minute), day1.Add(60*time.Minute), func(now time.Time) {
		if prev := now.Truncate(time.Minute).Add(-time.Minute); prev.Before(day1.Add(50 * time.Minute)) {
			a.store(ai(prev.Add(30*time.Second), wire.AIToolStart))
		}
	})
	a.finish(subFrom)

	a.runEvery(day2, day2.Add(50*time.Minute), func(now time.Time) {
		switch now.Truncate(time.Minute) {
		case day2.Add(15 * time.Minute):
			a.with(func(r *rig) {
				r.repos = []struggle.Repo{{Path: "/src/app", Project: "lib"}, {Path: "/src/lib", Project: "lib"}}
			})
		case day2.Add(25 * time.Minute):
			a.with(func(r *rig) { r.repos = both })
		}
		if now.Before(day2.Add(41 * time.Minute)) {
			a.busy(now)
		}
	})
	a.finish("2026-11-21")

	a.runEvery(day3, day3.Add(40*time.Minute), func(now time.Time) {
		if now.Truncate(time.Minute).Equal(day3.Add(20 * time.Minute)) {
			a.store(wire.LocalEvent{Type: "git", Git: &wire.GitEvent{Time: now.Add(-30 * time.Second), Project: "app",
				Kind: "commit", CommitHash: "1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d", FilesChanged: 3, Insertions: 40, Deletions: 2}})
		}
		if now.Before(day3.Add(31 * time.Minute)) {
			a.busy(now)
		}
	})
	a.finish(subTo)

	b := newRig(t, day3.Add(19*time.Minute).Add(5*time.Second), secondDevice(a, "desktop"))
	b.with(func(r *rig) { r.repos = []struggle.Repo{{Path: "/src/app", Project: "app"}} })
	b.runEvery(day3.Add(20*time.Minute), day3.Add(45*time.Minute), func(now time.Time) {
		if prev := now.Truncate(time.Minute).Add(-time.Minute); prev.Before(day3.Add(35 * time.Minute)) {
			b.present(prev, prev.Add(time.Minute))
			b.edit(prev.Add(20*time.Second), "app")
		}
	})
	b.finish(subTo)

	// Time at the keyboard for app, as heartbeats would count it: 110
	// minutes, ten of them with AI.
	seedtest.Span(t, a.st, a.user, day1, 40, seedtest.Minute{Project: "app"})
	seedtest.Span(t, a.st, a.user, day2, 30, seedtest.Minute{Project: "app"})
	seedtest.Span(t, a.st, a.user, day2.Add(30*time.Minute), 10, seedtest.Minute{Project: "app", Category: "ai coding"})
	seedtest.Span(t, a.st, a.user, day3, 30, seedtest.Minute{Project: "app"})
	return a, b
}

var (
	submittedRe = regexp.MustCompile(`as submission ([0-9a-f-]{36})`)
	reviewRe    = regexp.MustCompile(`Review link: (\S+)`)
)

// The whole flow of ADR 0020 with real parts on both sides: the laptop
// submits app for three days to a program; the job backs every one of its
// minutes and leaves the desktop's unbacked until the desktop adds its
// part; the review link serves a packet whose counts match the capped
// credit; a program reviewer records an adjusted decision that the
// submitter can't; tampered parts fail or stay unbacked as specified; and
// `logbook verify` on the bundle built from the server's stored data
// reproduces the packet's counts.
func TestSubmitReviewAndVerify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := workThreeDays(t)
	s := newSubServer(t, a)
	fiona := seedtest.User(t, a.st, "fiona", "UTC", true)
	rita := seedtest.User(t, a.st, "rita", "UTC", true)
	mallory := seedtest.User(t, a.st, "mallory", "UTC", true)
	app, err := s.oauth.CreateApp(ctx, fiona, oauth.AppInput{Name: "Terra", Slug: "terra",
		RedirectURIs: []string{"https://program.test/cb"}, Public: true, Confidential: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.oauth.AddMember(ctx, fiona, app.ID, "rita"); err != nil {
		t.Fatal(err)
	}
	req := submit.Request{Project: "app", From: subFrom, To: subTo, Program: "terra"}

	// --preview writes the exact body and uploads nothing.
	var out bytes.Buffer
	preview := filepath.Join(t.TempDir(), "my submission.json")
	pr := req
	pr.Preview = preview
	if err := submit.Run(ctx, pr, s.submitEnv(a, &out)); err != nil {
		t.Fatalf("preview: %v\n%s", err, out.String())
	}
	if n := s.count(`SELECT count(*) FROM submissions`); n != 0 {
		t.Fatalf("a preview uploaded %d submissions", n)
	}
	raw, err := os.ReadFile(preview) //nolint:gosec // the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	var honest submission.Upload
	if err := json.Unmarshal(raw, &honest); err != nil {
		t.Fatal(err)
	}
	if len(honest.Part.Blocks) != 3 || honest.Part.DeviceID != a.device.String() {
		t.Fatalf("preview part: %d blocks from %s", len(honest.Part.Blocks), honest.Part.DeviceID)
	}
	if !strings.Contains(out.String(), "      lib: ") || !strings.Contains(out.String(), "  desktop: ") {
		t.Fatalf("the summary doesn't name lib's revealed rows and the desktop's minutes:\n%s", out.String())
	}

	// Without --yes or a terminal, nothing is sent.
	if err := submit.Run(ctx, req, s.submitEnv(a, &out)); !errors.Is(err, submit.ErrNoTTY) {
		t.Fatalf("no tty: %v", err)
	}

	// Submit with --yes: the server freezes every device's rows and queues
	// the re-check; the agent prints the link and the --add command.
	out.Reset()
	req.Yes = true
	if err := submit.Run(ctx, req, s.submitEnv(a, &out)); err != nil {
		t.Fatalf("submit: %v\n%s", err, out.String())
	}
	m := submittedRe.FindStringSubmatch(out.String())
	link := reviewRe.FindStringSubmatch(out.String())
	if m == nil || link == nil || !strings.Contains(out.String(), "--add "+m[1]) {
		t.Fatalf("submit output:\n%s", out.String())
	}
	id := m[1]
	token, ok := strings.CutPrefix(link[1], reviewWeb+"/r/")
	if !ok {
		t.Fatalf("review link %s", link[1])
	}
	if rec := s.call(http.MethodGet, "/api/v1/review/"+token, uuid.Nil, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("review while verifying = %d %s", rec.Code, rec.Body)
	}
	s.work(1)

	aRows := s.count(`SELECT count(*) FROM credited_minutes WHERE device_id = $1 AND project = 'app'`, a.device)
	bRows := s.count(`SELECT count(*) FROM credited_minutes WHERE device_id = $1 AND project = 'app'`, b.device)
	if got := s.results(id, a.device); got["verified"] != aRows || len(got) != 1 || aRows < 60 {
		t.Fatalf("laptop rows = %v, want all %d verified", got, aRows)
	}
	if got := s.results(id, b.device); got["unbacked "+submission.ReasonNoPart] != bRows || len(got) != 1 || bRows < 10 {
		t.Fatalf("desktop rows = %v, want all %d unbacked with no part", got, bRows)
	}
	first := s.review(token)
	if first.Submission.Status != "ready" || first.Packet == nil || first.Decision != nil {
		t.Fatalf("review = %+v", first)
	}
	p := first.Packet
	wantPacket(t, s, id, p, true, b.device)
	if !strings.Contains(p.Justification, link[1]) {
		t.Fatalf("justification = %q", p.Justification)
	}

	// Deciding needs a member of the program who isn't the submitter.
	adjusted := map[string]any{"value": "adjusted", "approved_seconds": p.CountedSeconds / 2, "reason": "Two idle stretches"}
	decide := "/api/v1/review/" + token + "/decision"
	s.expect(http.MethodPost, decide, a.user, adjusted, http.StatusForbidden, nil)
	s.expect(http.MethodPost, decide, mallory, adjusted, http.StatusForbidden, nil)

	// The desktop adds its part: everything is re-checked and now backed.
	out.Reset()
	add := submit.Request{Project: "app", From: subFrom, To: subTo, AddTo: id, Yes: true}
	if err := submit.Run(ctx, add, s.submitEnv(b, &out)); err != nil {
		t.Fatalf("--add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Added this device's part to submission "+id) || s.status(id) != "verifying" {
		t.Fatalf("--add output (status %s):\n%s", s.status(id), out.String())
	}
	s.work(1)
	if got := s.results(id, b.device); got["verified"] != bRows || len(got) != 1 {
		t.Fatalf("desktop rows after its part = %v", got)
	}
	second := s.review(token)
	p2 := second.Packet
	if p2 == nil || p2.CountedSeconds != p.CountedSeconds {
		t.Fatalf("after the desktop's part: %+v", second)
	}
	wantPacket(t, s, id, p2, false, b.device)

	// A reviewer of the program records an adjusted decision; the
	// submitter still can't, and the desktop can't add another part.
	var decided struct {
		Decision struct {
			Value           string `json:"value"`
			ApprovedSeconds *int64 `json:"approved_seconds"`
			By              string `json:"by"`
		} `json:"decision"`
	}
	s.expect(http.MethodPost, decide, rita, adjusted, http.StatusOK, &decided)
	if d := decided.Decision; d.Value != "adjusted" || d.ApprovedSeconds == nil || *d.ApprovedSeconds != p.CountedSeconds/2 || d.By != "rita" {
		t.Fatalf("decision = %+v", decided)
	}
	s.expect(http.MethodPost, decide, a.user, map[string]any{"value": "approved"}, http.StatusForbidden, nil)
	if d := s.review(token).Decision; d == nil || d.Value != "adjusted" || d.By != "rita" {
		t.Fatalf("review decision = %+v", d)
	}
	if err := submit.Run(ctx, add, s.submitEnv(b, &out)); !errors.Is(err, serverapi.ErrConflict) {
		t.Fatalf("a part after the decision: %v", err)
	}

	// logbook verify on the bundle the admin API exports from the
	// server's stored data reproduces the packet's counts offline, and
	// the admin API shows the submission and decodes its revealed rows.
	bundle := wantVerifyReproduces(t, s, id, p2)
	wantAdminSubmission(t, s, id, bundle, 1)

	t.Run("edited line fails", func(t *testing.T) {
		up := cloneUpload(t, honest)
		edited := false
		for i := range up.Part.Blocks {
			for j, ob := range up.Part.Blocks[i].Opened {
				for k, line := range ob.Lines {
					if !edited && strings.Contains(line, "main.go") {
						up.Part.Blocks[i].Opened[j].Lines[k] = strings.Replace(line, "main.go", "main.rs", 1)
						edited = true
					}
				}
			}
		}
		if !edited {
			t.Fatal("no opened line names main.go")
		}
		res := s.upload(t, up)
		if st := s.status(res.ID); st != "failed" {
			t.Fatalf("status = %s", st)
		}
		if r := s.review(strings.TrimPrefix(res.ReviewURL, reviewWeb+"/r/")); r.Packet != nil || r.Submission.Status != "failed" {
			t.Fatalf("review of the failed submission = %+v", r)
		}
		var mine struct {
			Status string             `json:"status"`
			Error  string             `json:"error"`
			Packet *submission.Packet `json:"packet"`
		}
		s.expect(http.MethodGet, "/api/v1/me/submissions/"+res.ID, a.user, nil, http.StatusOK, &mine)
		if mine.Status != "failed" || !strings.Contains(mine.Error, `"laptop"`) || mine.Packet != nil {
			t.Fatalf("the submitter's view = %+v", mine)
		}
		s.expect(http.MethodGet, "/api/v1/me/submissions/"+res.ID, mallory, nil, http.StatusNotFound, nil)
	})

	t.Run("hidden window bucket is unbacked", func(t *testing.T) {
		up := cloneUpload(t, honest)
		hidden := hideBucket(t, a, &up)
		res := s.upload(t, up)
		if st := s.status(res.ID); st != "ready" {
			t.Fatalf("status = %s", st)
		}
		got := s.results(res.ID, a.device)
		if got["unbacked "+submission.ReasonIncomplete] == 0 || got["verified"] == 0 || len(got) != 2 {
			t.Fatalf("laptop rows with %s hidden = %v; want some unbacked as incomplete, the rest verified",
				hidden.Format(time.DateTime), got)
		}
		r := s.review(strings.TrimPrefix(res.ReviewURL, reviewWeb+"/r/"))
		if r.Packet == nil || r.Packet.Minutes.Mismatch != 0 || r.Packet.Minutes.Unbacked == 0 {
			t.Fatalf("packet = %+v", r.Packet)
		}
		wantVerifyReproduces(t, s, res.ID, r.Packet)

		// Once withdrawn, the raw rows are gone: the export and the
		// evidence are 410, the detail stays.
		s.expect(http.MethodDelete, "/api/v1/me/submissions/"+res.ID, a.user, nil, http.StatusNoContent, nil)
		for _, path := range []string{"/bundle", "/evidence"} {
			if rec := s.adminGet("/api/admin/v1/submissions/" + res.ID + path); rec.Code != http.StatusGone {
				t.Fatalf("%s after withdrawal = %d %s", path, rec.Code, rec.Body)
			}
		}
		if rec := s.adminGet("/api/admin/v1/submissions/" + res.ID); rec.Code != http.StatusOK ||
			!strings.Contains(rec.Body.String(), `"raw_deleted":true`) {
			t.Fatalf("detail after withdrawal = %d %s", rec.Code, rec.Body)
		}
	})
}

// wantPacket checks a ready packet against what the server froze: counted
// seconds are the capped credit of the view; minute counts and strips
// follow the frozen rows' results (some unbacked when desktopUnbacked);
// made_of adds up; cuts and flags carry their rules; and nothing private
// is in it.
func wantPacket(t *testing.T, s *subServer, id string, p *submission.Packet, desktopUnbacked bool, desktop uuid.UUID) {
	t.Helper()
	capped := s.count(`SELECT coalesce(sum(seconds), 0) FROM credited_project_minutes
		WHERE user_id = $1 AND project = 'app' AND minute >= $2 AND minute < $3`,
		s.r.user, day1.Truncate(24*time.Hour), day3.Truncate(24*time.Hour).AddDate(0, 0, 1))
	frozen := s.count(`SELECT coalesce(sum(seconds), 0) FROM submission_capped WHERE submission_id = $1`, id)
	if p.CountedSeconds != int64(capped) || frozen != capped || capped == 0 {
		t.Fatalf("counted %d s, frozen %d s, the view caps %d s", p.CountedSeconds, frozen, capped)
	}
	minutes := s.count(`SELECT count(DISTINCT minute) FROM submission_minutes WHERE submission_id = $1`, id)
	unbacked := s.count(`SELECT count(DISTINCT minute) FROM submission_minutes WHERE submission_id = $1 AND result = 'unbacked'`, id)
	if desktopUnbacked != (unbacked > 0) {
		t.Fatalf("%d unbacked minutes", unbacked)
	}
	if p.Minutes.Mismatch != 0 || p.Minutes.Unbacked != unbacked || p.Minutes.Verified != minutes-unbacked {
		t.Fatalf("minutes = %+v, want %d of %d unbacked", p.Minutes, unbacked, minutes)
	}

	// One strip per day, a character per minute. The counted ones are the
	// minutes with capped seconds or credit; those with an unbacked row
	// with credit are u, the rest y.
	counted := s.count(`SELECT count(DISTINCT sm.minute) FROM submission_minutes sm
		LEFT JOIN submission_capped c ON c.submission_id = sm.submission_id AND c.minute = sm.minute
		WHERE sm.submission_id = $1 AND (sm.credited > 0 OR c.seconds > 0)`, id)
	unbackedCounted := s.count(`SELECT count(DISTINCT minute) FROM submission_minutes
		WHERE submission_id = $1 AND result = 'unbacked' AND credited > 0`, id)
	if len(p.Days) != 3 || p.Days[0].Day != subFrom || p.Days[2].Day != subTo {
		t.Fatalf("days = %+v", p.Days)
	}
	var y, u, notCounted, commits int
	var daySum int64
	for _, d := range p.Days {
		if len(d.Strip) != 24*60 {
			t.Fatalf("%s strip has %d minutes", d.Day, len(d.Strip))
		}
		y += strings.Count(d.Strip, string(submission.StripCounted))
		u += strings.Count(d.Strip, string(submission.StripUnbacked))
		notCounted += strings.Count(d.Strip, string(submission.StripAgentAlone)) + strings.Count(d.Strip, string(submission.StripCut))
		if strings.ContainsRune(d.Strip, submission.StripMismatch) {
			t.Fatalf("%s strip has a mismatch", d.Day)
		}
		daySum += d.CountedSeconds
		for _, c := range d.Commits {
			commits++
			if c.Hash != "1a2b3c4" || c.Files != 3 || c.Added != 40 || c.Removed != 2 || d.Day != subTo {
				t.Fatalf("%s commit = %+v", d.Day, c)
			}
		}
	}
	if y+u != counted || u != unbackedCounted || daySum != p.CountedSeconds || commits != 1 {
		t.Fatalf("strips: %d y + %d u, want %d counted, %d unbacked; days sum to %d s of %d; %d commits",
			y, u, counted, unbackedCounted, daySum, p.CountedSeconds, commits)
	}
	if desktopUnbacked != (u > 0) || notCounted == 0 {
		t.Fatalf("strips: %d unbacked, %d cut or agent-alone minutes", u, notCounted)
	}
	if mo := p.MadeOf; mo.Editing+mo.WithAI+mo.Other != p.CountedSeconds || mo.Editing == 0 {
		t.Fatalf("made_of = %+v of %d s", mo, p.CountedSeconds)
	}
	if p.RawSeconds != 6600 || p.RawAISeconds != 600 || p.OneThirdRuleSeconds != 6600-600+200 {
		t.Fatalf("raw = %d s, AI %d s, one-third rule %d s", p.RawSeconds, p.RawAISeconds, p.OneThirdRuleSeconds)
	}
	if len(p.Cuts) == 0 {
		t.Fatal("no cuts in the packet")
	}
	for _, c := range p.Cuts {
		n := 0
		for _, r := range c.Ranges {
			n += int(r[1].Sub(r[0]) / time.Minute)
		}
		if c.Rule == "" || c.Rule != submission.CutRules[c.Code] || c.Seconds != int64(60*n) || c.Seconds == 0 {
			t.Fatalf("cut %+v", c)
		}
	}
	multi := slices.IndexFunc(p.Flags, func(f submission.FlagSummary) bool { return f.Code == struggle.FlagMultiDevice })
	if multi < 0 || p.Flags[multi].Minutes < 5 || p.Flags[multi].Rule != submission.FlagRules[struggle.FlagMultiDevice] {
		t.Fatalf("flags = %+v, want the minutes both devices credited", p.Flags)
	}
	var names []string
	for _, d := range p.Devices {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"desktop", "laptop"}) {
		t.Fatalf("devices = %v", names)
	}

	// Never in the packet: file paths, other projects, prompt hashes,
	// session ids, device ids.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/src/", "main.go", `"lib"`, promptHash, `"cc"`, "session_id", s.r.device.String(), desktop.String()} {
		if strings.Contains(string(b), private) {
			t.Fatalf("the packet holds %q:\n%s", private, b)
		}
	}
}

// adminGet calls the admin API with the auditor's admin key.
func (s *subServer) adminGet(target string) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+s.adminKeyFor())
	rec := httptest.NewRecorder()
	s.admin.ServeHTTP(rec, req)
	return rec
}

// wantVerifyReproduces downloads the submission's bundle from the admin
// API (GET /api/admin/v1/submissions/{id}/bundle, streamed from what the
// server stored) and checks that `logbook verify`'s check of the file
// passes with the packet's counts, and that the stream is the bundle the
// checker builds in memory. It returns the bundle.
func wantVerifyReproduces(t *testing.T, s *subServer, id string, p *submission.Packet) submission.Bundle {
	t.Helper()
	rec := s.adminGet("/api/admin/v1/submissions/" + id + "/bundle")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/gzip" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "submission-"+id+".json.gz") {
		t.Fatalf("bundle = %d %v", rec.Code, rec.Header())
	}
	path := filepath.Join(t.TempDir(), "bundle.json.gz")
	if err := os.WriteFile(path, rec.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read-only
	b, err := audit.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	rep := audit.Check(b)
	var text bytes.Buffer
	if err := rep.Write(&text); err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Minutes != p.Minutes || rep.CountedSeconds != p.CountedSeconds {
		t.Fatalf("logbook verify: %+v %d s; packet %+v %d s\n%s", rep.Minutes, rep.CountedSeconds, p.Minutes, p.CountedSeconds, text.String())
	}
	whole, err := s.checker.Bundle(context.Background(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	built, err := json.Marshal(whole)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed, built) {
		t.Fatal("the streamed bundle differs from the one built in memory")
	}
	return b
}

// wantAdminSubmission checks what the admin API shows of a submission:
// the detail with every frozen row, part and decision, and each part's
// revealed rows decoded, all of them, page by page.
func wantAdminSubmission(t *testing.T, s *subServer, id string, b submission.Bundle, decisions int) {
	t.Helper()
	var detail struct {
		Submission struct {
			Status string `json:"status"`
		} `json:"submission"`
		Packet    *submission.Packet `json:"packet"`
		Decisions []json.RawMessage  `json:"decisions"`
		Parts     []json.RawMessage  `json:"parts"`
		Minutes   []struct {
			Result *string `json:"result"`
		} `json:"minutes"`
	}
	rec := s.adminGet("/api/admin/v1/submissions/" + id)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Submission.Status != "ready" || detail.Packet == nil || len(detail.Decisions) != decisions ||
		len(detail.Parts) != len(b.Parts) || len(detail.Minutes) != len(b.Minutes) {
		t.Fatalf("detail: status %s, %d decisions, %d parts, %d minutes", detail.Submission.Status,
			len(detail.Decisions), len(detail.Parts), len(detail.Minutes))
	}
	for _, m := range detail.Minutes {
		if m.Result == nil {
			t.Fatal("a frozen row has no result")
		}
	}
	for _, part := range b.Parts {
		lines := 0
		for _, blk := range part.Blocks {
			for _, o := range blk.Opened {
				lines += len(o.Lines)
			}
		}
		var seen []string
		for offset := 0; ; offset += 500 {
			var page struct {
				Rows []struct {
					Day   string          `json:"day"`
					Seq   int64           `json:"seq"`
					Type  string          `json:"type"`
					Value json.RawMessage `json:"value"`
				} `json:"rows"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			}
			rec := s.adminGet(fmt.Sprintf("/api/admin/v1/submissions/%s/evidence?part=%s&limit=500&offset=%d", id, part.DeviceID, offset))
			if rec.Code != http.StatusOK {
				t.Fatalf("evidence = %d %s", rec.Code, rec.Body)
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if page.Total != lines {
				t.Fatalf("evidence total = %d, the part opens %d lines", page.Total, lines)
			}
			for _, r := range page.Rows {
				if r.Day == "" || r.Seq <= 0 || r.Type == "" || len(r.Value) == 0 {
					t.Fatalf("evidence row %+v", r)
				}
				seen = append(seen, r.Type)
			}
			if !page.HasMore {
				break
			}
		}
		if len(seen) != lines || !slices.Contains(seen, "heartbeat") {
			t.Fatalf("read %d evidence rows, want %d with heartbeats among them", len(seen), lines)
		}
	}
}

// uploaded is the server's answer to an upload.
type uploaded struct {
	ID, ReviewURL string
}

// upload sends up as the laptop's agent would, then works its re-check.
func (s *subServer) upload(t *testing.T, up submission.Upload) uploaded {
	t.Helper()
	body, err := json.Marshal(up)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.client.Submit(context.Background(), s.target, body)
	if err != nil {
		t.Fatal(err)
	}
	s.work(1)
	return uploaded{ID: res.ID, ReviewURL: res.ReviewURL}
}

func cloneUpload(t *testing.T, up submission.Upload) submission.Upload {
	t.Helper()
	b, err := json.Marshal(up)
	if err != nil {
		t.Fatal(err)
	}
	var out submission.Upload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// hideBucket rebuilds one block's proof in up without an opened minute
// bucket that lies in a listed minute's window: the proof still verifies
// against the signed record, but can't show that window complete. It
// returns the hidden bucket's minute.
func hideBucket(t *testing.T, r *rig, up *submission.Upload) time.Time {
	t.Helper()
	stored := r.serverDays()
	for bi, blk := range up.Part.Blocks {
		i := slices.IndexFunc(stored, func(s ledger.StoredDay) bool { return s.Record.Day == blk.Day })
		if i < 0 {
			t.Fatalf("no stored record for %s", blk.Day)
		}
		v, err := evidence.VerifyProof(blk.Proof, stored[i].Record)
		if err != nil {
			t.Fatal(err)
		}
		for _, ob := range v.Opened {
			if ob.Key == evidence.RepoKey {
				continue
			}
			at := evidence.KeyMinute(ob.Key)
			if !slices.ContainsFunc(up.Part.Minutes, func(pm submission.PartMinute) bool {
				w := window(pm.Key())
				return !at.Before(w.From) && at.Before(w.To)
			}) {
				continue
			}
			open := map[int][]evidence.Row{}
			for _, o := range v.Opened {
				if o.I != ob.I {
					open[o.I] = o.Rows
				}
			}
			var edges []int
			for _, e := range blk.Edges {
				edges = append(edges, e.I)
			}
			proof, err := evidence.BuildProof(r.bucketList(blk.Day), open, edges)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := evidence.VerifyProof(proof, stored[i].Record); err != nil {
				t.Fatalf("the proof without bucket %d should still verify: %v", ob.I, err)
			}
			up.Part.Blocks[bi].Proof = proof
			return at
		}
	}
	t.Fatal("no opened minute bucket lies in a listed minute's window")
	return time.Time{}
}
