package submissions_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/pkg/submission"
)

// The plan lists every device's rows of the project and range, and
// refuses a range ending after a day some device hasn't sealed.
func TestPlan(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	target := "/api/v1/me/submissions/plan?project=app&from=" + submittedDay + "&to=" + submittedDay + "&device=" + w.a.id.String()
	p := decode[submission.Plan](t, w.expect(call{target: target, user: w.alice}, http.StatusOK))
	if len(p.Devices) != 2 || p.Project != "app" {
		t.Fatalf("devices = %+v", p.Devices)
	}
	for _, d := range p.Devices {
		if d.SealedThrough == nil || *d.SealedThrough != submittedDay || d.ThisDevice != (d.ID == w.a.id.String()) {
			t.Fatalf("device = %+v", d)
		}
	}
	var appRows int
	for _, m := range w.a.minutes {
		if m.Project == "app" {
			appRows++
		}
	}
	if len(p.Minutes) != appRows+5 {
		t.Fatalf("minutes = %d, want %d", len(p.Minutes), appRows+5)
	}
	for _, m := range p.Minutes {
		if m.Project != "app" || m.ComputedAt == nil || m.Snapshot == nil {
			t.Fatalf("minute = %+v", m)
		}
	}
	if !slices.IsSortedFunc(p.Minutes, func(a, b submission.Minute) int {
		if a.DeviceID != b.DeviceID {
			if a.DeviceID < b.DeviceID {
				return -1
			}
			return 1
		}
		return a.Minute.Compare(b.Minute)
	}) {
		t.Fatal("minutes not sorted by (device, minute)")
	}

	for _, q := range []string{
		"project=app&from=2026-11-20&to=2026-11-21", // after the sealed day
		"project=&from=2026-11-20&to=2026-11-20",
		"project=app&from=2026-11-20",
		"project=app&from=2026-11-20&to=2026-11-20&device=laptop",
	} {
		w.expect(call{target: "/api/v1/me/submissions/plan?" + q, user: w.alice}, http.StatusBadRequest)
	}
	// Another user sees none of alice's rows, even naming her device.
	p = decode[submission.Plan](t, w.expect(call{target: target, user: w.bob}, http.StatusOK))
	if len(p.Minutes) != 0 || len(p.Devices) != 0 {
		t.Fatalf("bob's plan = %+v", p)
	}
	w.expect(call{target: target}, http.StatusUnauthorized)
}

// Withdrawing: the link answers 410, the raw rows are deleted, it is
// audited, and it can't be undone or done by anyone else.
func TestWithdraw(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	target := "/api/v1/me/submissions/" + created.ID.String()

	w.expect(call{method: http.MethodDelete, target: target, user: w.bob}, http.StatusNotFound)
	w.expect(call{method: http.MethodDelete, target: target, user: w.alice}, http.StatusNoContent)
	if n := w.count(`SELECT count(*) FROM submission_parts WHERE submission_id = $1 AND data IS NULL AND raw_deleted_at IS NOT NULL`,
		created.ID); n != 1 {
		t.Fatal("raw rows kept after withdrawal")
	}
	rec := w.do(call{target: "/api/v1/review/" + token})
	if rec.Code != http.StatusGone || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("withdrawn review = %d", rec.Code)
	}
	w.decide(w.rita, token, `{"value": "approved"}`, http.StatusGone)
	w.expect(call{method: http.MethodDelete, target: target, user: w.alice}, http.StatusNoContent) // again: no-op
	d := decode[detail](t, w.expect(call{target: target, user: w.alice}, http.StatusOK))
	if d.Status != "withdrawn" || d.ReviewURL != nil || d.Packet != nil {
		t.Fatalf("withdrawn detail = %+v", d)
	}
	if a := w.audit(w.alice); len(a) != 2 || a[1].Action != "submission.withdraw" {
		t.Fatalf("audit = %+v", a)
	}
	// A part can't be added to it, and a re-check doesn't revive it.
	part := mustJSON(t, submission.Part{DeviceID: w.b.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}})
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + created.ID.String() + "/parts", user: w.alice,
		body: part, gz: true}, http.StatusConflict)
	w.verify(created.ID)
	if w.status(created.ID) != "withdrawn" {
		t.Fatal("re-check revived a withdrawn submission")
	}
}

// Every endpoint that reads a submission is scoped to its submitter, its
// program's members, or its program's own client token.
func TestAccessIsScoped(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	other := w.program(w.mallory, "other", true)
	id := created.ID.String()

	// The submitter's own endpoints.
	mine := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/me/submissions", user: w.alice}, http.StatusOK))["submissions"]
	if len(mine) != 1 || mine[0].ID != created.ID || mine[0].ReviewURL == nil || mine[0].Packet != nil {
		t.Fatalf("alice's list = %+v", mine)
	}
	if got := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/me/submissions", user: w.bob}, http.StatusOK))["submissions"]; len(got) != 0 {
		t.Fatalf("bob's list = %+v", got)
	}
	w.expect(call{target: "/api/v1/me/submissions/" + id, user: w.bob}, http.StatusNotFound)
	w.expect(call{target: "/api/v1/me/submissions/not-a-uuid", user: w.alice}, http.StatusNotFound)
	w.expect(call{target: "/api/v1/me/submissions"}, http.StatusUnauthorized)
	part := mustJSON(t, submission.Part{DeviceID: w.b.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}})
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + id + "/parts", user: w.bob, body: part, gz: true}, http.StatusBadRequest)
	bobPart := mustJSON(t, submission.Part{DeviceID: w.register(w.bob, "bobs", 3).id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}})
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + id + "/parts", user: w.bob, body: bobPart, gz: true}, http.StatusNotFound)

	// The program's members see it in the app's inbox; others don't.
	for _, u := range []uuid.UUID{w.fiona, w.rita} {
		got := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/me/apps/" + w.terra.String() + "/submissions", user: u}, http.StatusOK))["submissions"]
		if len(got) != 1 || got[0].ID != created.ID || got[0].ReviewURL == nil || got[0].User == nil ||
			(*got[0].User)["username"] != "alice" || got[0].Error != "" {
			t.Fatalf("member's inbox = %+v", got)
		}
	}
	w.expect(call{target: "/api/v1/me/apps/" + w.terra.String() + "/submissions", user: w.mallory}, http.StatusNotFound)
	w.expect(call{target: "/api/v1/me/apps/" + w.terra.String() + "/submissions", user: w.alice}, http.StatusNotFound)
	if got := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/me/apps/" + other.String() + "/submissions", user: w.mallory}, http.StatusOK))["submissions"]; len(got) != 0 {
		t.Fatalf("other program's inbox = %+v", got)
	}

	// The program's client token reads only its own submissions.
	got := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/submissions", app: w.terra}, http.StatusOK))["submissions"]
	if len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("terra's list = %+v", got)
	}
	one := decode[detail](t, w.expect(call{target: "/api/v1/submissions/" + id, app: w.terra}, http.StatusOK))
	if one.Packet == nil || one.Packet.Justification == "" || one.User == nil {
		t.Fatalf("terra's submission = %+v", one)
	}
	if got := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/submissions", app: other}, http.StatusOK))["submissions"]; len(got) != 0 {
		t.Fatalf("other app's list = %+v", got)
	}
	w.expect(call{target: "/api/v1/submissions/" + id, app: other}, http.StatusNotFound)
	w.expect(call{target: "/api/v1/submissions/" + id}, http.StatusUnauthorized)

	// Filters.
	for q, n := range map[string]int{
		"?status=ready": 1, "?status=failed": 0, "?decision=none": 1, "?decision=approved": 0, "?limit=1": 1, "?offset=1": 0,
	} {
		if got := decode[map[string][]detail](t, w.expect(call{target: "/api/v1/submissions" + q, app: w.terra}, http.StatusOK))["submissions"]; len(got) != n {
			t.Errorf("%s = %d submissions, want %d", q, len(got), n)
		}
	}
	for _, q := range []string{"?status=nope", "?decision=maybe", "?limit=0", "?limit=501", "?offset=-1", "?limit=x"} {
		w.expect(call{target: "/api/v1/submissions" + q, app: w.terra}, http.StatusBadRequest)
	}

	// The review link needs no sign-in; a token that isn't one is 404.
	w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK)
	w.expect(call{target: "/api/v1/review/nope"}, http.StatusNotFound)
	w.expect(call{target: "/api/v1/review/AAAAAAAAAAAAAAAAAAAAAA"}, http.StatusNotFound)
}

// The review link is never cached, sent as a referrer or indexed, on
// every answer.
func TestReviewHeaders(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	check := func(rec interface{ Header() http.Header }, what string) {
		t.Helper()
		h := rec.Header()
		if h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Robots-Tag") != "noindex" {
			t.Errorf("%s headers = %v", what, h)
		}
	}
	check(w.do(call{target: "/api/v1/review/" + token}), "verifying")
	w.verify(created.ID)
	check(w.do(call{target: "/api/v1/review/" + token}), "ready")
	check(w.do(call{target: "/api/v1/review/AAAAAAAAAAAAAAAAAAAAAA"}), "unknown")
	check(w.do(call{method: http.MethodPost, target: "/api/v1/review/" + token + "/decision", user: w.mallory, body: []byte(`{"value":"approved"}`)}), "decision")
	if rec := w.do(call{target: "/api/v1/me/submissions", user: w.alice}); rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the submitter's list can be cached")
	}
}

// Raw rows are deleted a year after the latest decision, or after the
// submission when there is none.
func TestRawRowRetention(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	first, _ := w.submit("terra")
	second, token := w.submit("terra")
	w.verify(first.ID)
	w.verify(second.ID)
	w.clock = now.AddDate(0, 6, 0)
	w.decide(w.rita, token, `{"value": "approved"}`, http.StatusOK)

	raw := func(id uuid.UUID) bool {
		return w.count(`SELECT count(*) FROM submission_parts WHERE submission_id = $1 AND data IS NOT NULL`, id) == 1
	}
	w.clock = now.AddDate(1, 0, 0).Add(-time.Minute)
	if n, err := w.checker.ExpireRaw(ctx); err != nil || n != 0 {
		t.Fatalf("expired early: %d, %v", n, err)
	}
	w.clock = now.AddDate(1, 0, 1)
	if n, err := w.checker.ExpireRaw(ctx); err != nil || n != 1 || raw(first.ID) || !raw(second.ID) {
		t.Fatalf("after a year: %d, %v", n, err)
	}
	w.clock = now.AddDate(1, 6, 1)
	if n, err := w.checker.ExpireRaw(ctx); err != nil || n != 1 || raw(second.ID) {
		t.Fatalf("a year after the decision: %d, %v", n, err)
	}
	// The packet and decision stay.
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if r.Packet == nil || r.Decision == nil {
		t.Fatal("packet or decision deleted with the raw rows")
	}
}
