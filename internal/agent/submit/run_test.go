package submit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/pkg/submission"
)

// fakeServer answers the plan and records uploads.
type fakeServer struct {
	plan    submission.Plan
	planErr error
	queries []serverapi.PlanQuery
	// submitted and added are the bodies uploaded, and addedTo the ids.
	submitted, added [][]byte
	addedTo          []string
}

func (s *fakeServer) Plan(_ context.Context, _ serverapi.Target, q serverapi.PlanQuery) (submission.Plan, error) {
	s.queries = append(s.queries, q)
	return s.plan, s.planErr
}

func (s *fakeServer) Submit(_ context.Context, _ serverapi.Target, body []byte) (serverapi.Submitted, error) {
	s.submitted = append(s.submitted, body)
	return serverapi.Submitted{ID: "c0000000-0000-4000-8000-000000000003", Status: "verifying",
		ReviewURL: "https://logbook.boogles.dev/r/tok"}, nil
}

func (s *fakeServer) AddPart(_ context.Context, _ serverapi.Target, id string, body []byte) (serverapi.Submitted, error) {
	s.added, s.addedTo = append(s.added, body), append(s.addedTo, id)
	return serverapi.Submitted{ID: id}, nil
}

func (s *fakeServer) uploads() int { return len(s.submitted) + len(s.added) }

func env(f *fixture, srv *fakeServer, in string, interactive bool, out *bytes.Buffer) Env {
	srv.plan = f.plan
	return Env{Server: srv, Target: serverapi.Target{BaseURL: "https://api.example", APIKey: "k"}, DeviceID: laptop, Prove: f.prove,
		In: strings.NewReader(in), Out: out, Interactive: interactive, Now: func() time.Time { return now }}
}

func request() Request { return Request{Project: "app", From: "2026-11-20", To: "2026-11-20"} }

// --preview writes the exact body to the file (a path with a space and a
// new folder's worth of separators, built with filepath) and uploads
// nothing; it needs no confirmation.
func TestPreviewWritesAndDoesntUpload(t *testing.T) {
	f := newFixture(t)
	srv := &fakeServer{}
	var out bytes.Buffer
	path := filepath.Join(t.TempDir(), "my bundle.json")
	req := request()
	req.Preview, req.Program = path, "terra"
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil {
		t.Fatal(err)
	}
	if srv.uploads() != 0 {
		t.Fatal("--preview uploaded")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var up submission.Upload
	if err := json.Unmarshal(raw, &up); err != nil {
		t.Fatal(err)
	}
	if err := up.Validate(); err != nil || up.Program != "terra" || up.Part.DeviceID != laptop || len(up.Part.Minutes) != len(f.own())-2 {
		t.Fatalf("preview = %+v, %v", up, err)
	}
	if !strings.Contains(out.String(), "Nothing was uploaded") || !strings.Contains(out.String(), "lib: 3 rows") {
		t.Fatalf("output:\n%s", out.String())
	}
	if st, err := os.Stat(path); err != nil || st.Size() != int64(len(raw)) {
		t.Fatalf("stat = %v, %v", st, err)
	}
}

// Without a terminal to confirm on, submit refuses unless --yes, before it
// asks the server for anything.
func TestNoTTYWithoutYesRefuses(t *testing.T) {
	f := newFixture(t)
	srv := &fakeServer{}
	var out bytes.Buffer
	err := Run(context.Background(), request(), env(f, srv, "yes\n", false, &out))
	if !errors.Is(err, ErrNoTTY) || len(srv.queries) != 0 || srv.uploads() != 0 {
		t.Fatalf("err = %v, %d plan calls, %d uploads", err, len(srv.queries), srv.uploads())
	}
	req := request()
	req.Yes = true
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil || len(srv.submitted) != 1 {
		t.Fatalf("--yes: err = %v, %d uploads", err, len(srv.submitted))
	}
	if !strings.Contains(out.String(), "Review link: https://logbook.boogles.dev/r/tok") ||
		!strings.Contains(out.String(), "--add c0000000-0000-4000-8000-000000000003") {
		t.Fatalf("output:\n%s", out.String())
	}
	if q := srv.queries[0]; q != (serverapi.PlanQuery{Project: "app", From: "2026-11-20", To: "2026-11-20", DeviceID: laptop}) {
		t.Fatalf("plan query = %+v", q)
	}
}

// On a terminal the user is asked; only y or yes uploads, and end of
// input is no.
func TestConfirmation(t *testing.T) {
	for answer, uploads := range map[string]int{"y\n": 1, " YES \n": 1, "n\n": 0, "": 0, "sure\n": 0} {
		f := newFixture(t)
		srv := &fakeServer{}
		var out bytes.Buffer
		err := Run(context.Background(), request(), env(f, srv, answer, true, &out))
		if srv.uploads() != uploads || (uploads == 0) != errors.Is(err, ErrCancelled) {
			t.Errorf("answer %q: %d uploads, err %v", answer, srv.uploads(), err)
		}
		if !strings.Contains(out.String(), "Upload this to https://api.example? [y/N]") {
			t.Errorf("answer %q: no prompt in\n%s", answer, out.String())
		}
	}
}

// The uploaded body is the submission.Upload; --add sends only the part,
// to the parts endpoint.
func TestUploadBodies(t *testing.T) {
	f := newFixture(t)
	srv := &fakeServer{}
	var out bytes.Buffer
	req := request()
	req.Yes = true
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil {
		t.Fatal(err)
	}
	var up submission.Upload
	if err := json.Unmarshal(srv.submitted[0], &up); err != nil || up.Validate() != nil || up.Program != "" ||
		strings.Contains(string(srv.submitted[0]), `"program"`) {
		t.Fatalf("upload = %s, %v", srv.submitted[0], err)
	}

	req.AddTo = "C0000000-0000-4000-8000-000000000003"
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil {
		t.Fatal(err)
	}
	var part submission.Part
	if err := json.Unmarshal(srv.added[0], &part); err != nil || part.Validate() != nil || part.DeviceID != laptop {
		t.Fatalf("part = %s, %v", srv.added[0], err)
	}
	if srv.addedTo[0] != "c0000000-0000-4000-8000-000000000003" {
		t.Fatalf("added to %q", srv.addedTo[0])
	}
	if strings.Contains(string(srv.added[0]), `"project"`) {
		t.Fatal("--add sent the upload, not the part")
	}
}

// A range ending today or later, or past a device's last sealed day, is
// refused with the plain reason before anything is uploaded.
func TestRangeRefused(t *testing.T) {
	tests := map[string]struct {
		req  func(r *Request)
		plan func(p *submission.Plan)
		want error
		text string
	}{
		"today":       {req: func(r *Request) { r.To = "2026-11-25" }, want: ErrToday, text: "end it on 2026-11-24 or earlier"},
		"after today": {req: func(r *Request) { r.From, r.To = "2026-11-24", "2026-11-30" }, want: ErrToday},
		"not sealed": {req: func(r *Request) { r.From, r.To = "2026-11-20", "2026-11-23" },
			plan: func(p *submission.Plan) {
				d := "2026-11-21"
				p.Devices[1].SealedThrough = &d
			}, want: ErrNotSealed, text: "desktop has sealed its evidence through 2026-11-21"},
		"never sealed": {plan: func(p *submission.Plan) { p.Devices[1].SealedThrough = nil }, want: ErrNotSealed, text: "desktop has sealed no day yet"},
		"nothing":      {plan: func(p *submission.Plan) { p.Minutes = nil }, want: ErrNothing},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if tt.plan != nil {
				tt.plan(&f.plan)
			}
			srv := &fakeServer{}
			var out bytes.Buffer
			req := request()
			req.Yes = true
			if tt.req != nil {
				tt.req(&req)
			}
			err := Run(context.Background(), req, env(f, srv, "", false, &out))
			if !errors.Is(err, tt.want) || srv.uploads() != 0 || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("err = %v, %d uploads", err, srv.uploads())
			}
		})
	}
}

// The server's 400 for a range it can't take is passed on with its reason.
func TestPlanRefusedPassesReason(t *testing.T) {
	f := newFixture(t)
	srv := &fakeServer{}
	var out bytes.Buffer
	e := env(f, srv, "", false, &out)
	srv.planErr = &serverapi.SubmitError{Reason: serverapi.ErrRefused,
		Cause: &serverapi.Error{Kind: serverapi.KindInvalid, Status: 400, Message: "the range can end on 2026-11-19 at the latest"}}
	req := request()
	req.Yes = true
	err := Run(context.Background(), req, e)
	if !errors.Is(err, serverapi.ErrRefused) || !strings.Contains(err.Error(), "can end on 2026-11-19") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestChecks(t *testing.T) {
	for name, edit := range map[string]func(r *Request){
		"no project":      func(r *Request) { r.Project = " \t" },
		"bad day":         func(r *Request) { r.From = "2026-11-31" },
		"reversed":        func(r *Request) { r.From, r.To = "2026-11-20", "2026-11-19" },
		"too long":        func(r *Request) { r.From = "2026-01-01" },
		"bad id":          func(r *Request) { r.AddTo = "123" },
		"add and program": func(r *Request) { r.AddTo, r.Program = "c0000000-0000-4000-8000-000000000003", "terra" },
	} {
		r := request()
		edit(&r)
		if err := r.check(now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A refused range says why: the bad day and the format, the order, or the
// longest range.
func TestRangeErrorsSayWhy(t *testing.T) {
	for name, tc := range map[string]struct {
		from, to string
		want     string
	}{
		"bad format": {"2026-11-1", "2026-11-20", `--from "2026-11-1" is not a day (days are YYYY-MM-DD, UTC)`},
		"bad to":     {"2026-11-01", "20-11-2026", `--to "20-11-2026" is not a day`},
		"reversed":   {"2026-11-20", "2026-11-01", "--to is before --from"},
		"too long":   {"2026-01-01", "2026-11-20", "the range is 324 days; submit at most 92 days at a time"},
	} {
		r := request()
		r.From, r.To = tc.from, tc.to
		err := r.check(now)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "malformed") {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// --add asks for the submission's own plan (its frozen rows), so the part
// lists only rows the server froze; a plain submit asks for the live one.
func TestAddUsesTheSubmissionsPlan(t *testing.T) {
	f := newFixture(t)
	srv := &fakeServer{}
	var out bytes.Buffer
	req := request()
	req.Yes = true
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil {
		t.Fatal(err)
	}
	req.AddTo = "c0000000-0000-4000-8000-000000000003"
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil {
		t.Fatal(err)
	}
	if len(srv.queries) != 2 || srv.queries[0].Submission != "" || srv.queries[1].Submission != req.AddTo {
		t.Fatalf("plan queries = %+v", srv.queries)
	}
	// The server's answer for a submission that takes no parts names it.
	srv.planErr = &serverapi.SubmitError{Reason: serverapi.ErrConflict,
		Cause: &serverapi.Error{Kind: serverapi.KindConflict, Status: 409, Message: "the submission was withdrawn"}}
	err := Run(context.Background(), req, env(f, srv, "", false, &out))
	if !errors.Is(err, serverapi.ErrConflict) || !strings.Contains(err.Error(), req.AddTo) || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("err = %v", err)
	}
}

// A revoked device can't seal more days, so it doesn't hold the range
// back; an unrevoked one that hasn't sealed it says revoking would help.
func TestRevokedDeviceDoesntHoldRangeBack(t *testing.T) {
	f := newFixture(t)
	f.plan.Devices[1].SealedThrough = nil
	srv := &fakeServer{}
	var out bytes.Buffer
	req := request()
	req.Yes = true
	err := Run(context.Background(), req, env(f, srv, "", false, &out))
	if !errors.Is(err, ErrNotSealed) || !strings.Contains(err.Error(), "revoke it on your Logbook dashboard") {
		t.Fatalf("err = %v", err)
	}
	f.plan.Devices[1].Revoked = true
	if err := Run(context.Background(), req, env(f, srv, "", false, &out)); err != nil || srv.uploads() != 1 {
		t.Fatalf("with the device revoked: %v, %d uploads", err, srv.uploads())
	}
}
