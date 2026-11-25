package submissions_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/pkg/submission"
)

type detail struct {
	ID             uuid.UUID          `json:"id"`
	Status         string             `json:"status"`
	Error          string             `json:"error"`
	CountedSeconds int64              `json:"counted_seconds"`
	RawSeconds     int64              `json:"raw_seconds"`
	Minutes        submission.Counts  `json:"minutes"`
	ReviewURL      *string            `json:"review_url"`
	Program        *map[string]string `json:"program"`
	Decision       *map[string]any    `json:"decision"`
	Packet         *submission.Packet `json:"packet"`
	User           *map[string]any    `json:"user"`
}

type review struct {
	Submission map[string]any     `json:"submission"`
	Packet     *submission.Packet `json:"packet"`
	Decision   *map[string]any    `json:"decision"`
}

// The whole honest path: alice submits device A's part; device B's rows
// have no part. The real verifier backs A's minutes, B's are unbacked, the
// cap was frozen across both devices, and the review link serves the
// packet with its link-bearing fields filled in.
func TestSubmitVerifyAndReview(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	created, token := w.submit("terra")

	// The re-check is queued in River's queue for submissions.
	if n := w.count(`SELECT count(*) FROM river_job WHERE kind = 'submission_verify' AND queue = 'submissions'
		AND args->>'submission_id' = $1`, created.ID.String()); n != 1 {
		t.Fatalf("queued jobs = %d", n)
	}
	// Until it runs, the link answers 202 with the submission only.
	rec := w.do(call{target: "/api/v1/review/" + token})
	if rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "packet") {
		t.Fatalf("verifying review = %d %s", rec.Code, rec.Body)
	}

	w.verify(created.ID)
	if s := w.status(created.ID); s != "ready" {
		t.Fatalf("status = %s", s)
	}
	// A's frozen app rows are verified; B's are unbacked, with why.
	var appRowsA int
	for _, m := range w.a.minutes {
		if m.Project == "app" {
			appRowsA++
		}
	}
	if n := w.count(`SELECT count(*) FROM submission_minutes WHERE submission_id = $1 AND device_id = $2 AND result = 'verified'`,
		created.ID, w.a.id); n != appRowsA || n == 0 {
		t.Fatalf("A verified rows = %d, want %d", n, appRowsA)
	}
	if n := w.count(`SELECT count(*) FROM submission_minutes WHERE submission_id = $1 AND device_id = $2
		AND result = 'unbacked' AND reason = $3`, created.ID, w.b.id, submission.ReasonNoPart); n != 5 {
		t.Fatalf("B unbacked rows = %d", n)
	}

	// The cap was frozen across both devices: at 09:30 B's 30 s of lib
	// share the minute with A's 60 s of app (60 * 60 / 90 = 40), and at
	// 09:40 both devices credit app (capped at 60).
	capped := func(m time.Time) int {
		return w.count(`SELECT seconds FROM submission_capped WHERE submission_id = $1 AND minute = $2`, created.ID, m)
	}
	if capped(at(9, 30)) != 40 || capped(at(9, 40)) != 60 {
		t.Fatalf("capped 09:30 = %d, 09:40 = %d", capped(at(9, 30)), capped(at(9, 40)))
	}
	if n := w.count(`SELECT count(*) FROM submission_cap_inputs WHERE submission_id = $1 AND device_id = $2 AND project = 'lib'`,
		created.ID, w.b.id); n != 1 {
		t.Fatalf("B's lib cap inputs = %d", n)
	}
	// Changing credited rows later doesn't change what was frozen.
	w.exec(`DELETE FROM credited_minutes WHERE device_id = $1 AND project = 'lib'`, w.b.id)
	if capped(at(9, 30)) != 40 {
		t.Fatal("frozen capped seconds changed with credited_minutes")
	}

	body := w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK)
	r := decode[review](t, body)
	p := r.Packet
	if p == nil {
		t.Fatalf("no packet: %s", body)
	}
	var wantCounted int64
	if err := w.st.Pool.QueryRow(ctx, `SELECT sum(seconds) FROM submission_capped WHERE submission_id = $1`, created.ID).Scan(&wantCounted); err != nil {
		t.Fatal(err)
	}
	if p.CountedSeconds != wantCounted || p.Minutes.Unbacked != 5 || p.Minutes.Mismatch != 0 || p.Minutes.Verified == 0 {
		t.Fatalf("packet counts = %d s, %+v; want %d s", p.CountedSeconds, p.Minutes, wantCounted)
	}
	if p.RawSeconds != 3600 || p.RawAISeconds != 600 || p.OneThirdRuleSeconds != 3000+200 {
		t.Fatalf("raw = %d, ai = %d, one third = %d", p.RawSeconds, p.RawAISeconds, p.OneThirdRuleSeconds)
	}
	if p.Trust.AtSubmission != "green" || p.Trust.Now != "green" || p.Trust.CheckedAt == nil || !p.Trust.HackatimeLinked {
		t.Fatalf("trust = %+v", p.Trust)
	}
	link := webURL + "/r/" + token
	if !strings.HasSuffix(p.Justification, "Review: "+link) || p.RawRowsDeletedAfter != "2027-11-22" {
		t.Fatalf("justification = %q, deleted after %q", p.Justification, p.RawRowsDeletedAfter)
	}
	if len(p.Days) != 1 || len(p.Days[0].Commits) != 1 || p.Days[0].Commits[0].Hash != "0123456" {
		t.Fatalf("days = %+v", p.Days)
	}
	if u := r.Submission["user"].(map[string]any); u["username"] != "alice" || u["hackatime_id"] != float64(4321) {
		t.Fatalf("user = %v", u)
	}
	if prog := r.Submission["program"].(map[string]any); prog["slug"] != "terra" {
		t.Fatalf("program = %v", prog)
	}
	if r.Decision != nil {
		t.Fatalf("decision = %v", *r.Decision)
	}

	// The packet never shows file paths, other projects, prompt hashes,
	// session ids or device ids, and the stored one never holds the link.
	for _, secret := range []string{"/src/", `"lib"`, "deadbeefcafe", "secret-session", w.a.id.String(), w.b.id.String()} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("review page shows %q", secret)
		}
	}
	if n := w.count(`SELECT count(*) FROM submissions WHERE id = $1 AND (packet::text LIKE '%' || $2 || '%' OR packet ? 'justification')`,
		created.ID, token); n != 0 {
		t.Error("stored packet holds the link")
	}

	// The token is stored only as its SHA-256 and encrypted.
	sum := sha256.Sum256([]byte(token))
	if n := w.count(`SELECT count(*) FROM submissions WHERE id = $1 AND token_hash = $2`, created.ID, sum[:]); n != 1 {
		t.Fatal("token hash not stored")
	}
	if n := w.count(`SELECT count(*) FROM submissions WHERE id = $1 AND position(convert_to($2, 'UTF8') IN token_enc) > 0`,
		created.ID, token); n != 0 {
		t.Fatal("token stored in plain text")
	}
	if n := w.count(`SELECT count(*) FROM audit_log WHERE data::text LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Fatal("token in the audit log")
	}
	audit := w.audit(w.alice)
	if len(audit) != 1 || audit[0].Action != "submission.create" || audit[0].SubjectID != created.ID.String() {
		t.Fatalf("audit = %+v", audit)
	}

	// Re-running the job changes nothing: the packet is the same.
	var before []byte
	if err := w.st.Pool.QueryRow(ctx, `SELECT packet FROM submissions WHERE id = $1`, created.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w.exec(`UPDATE submissions SET status = 'verifying' WHERE id = $1`, created.ID)
	w.verify(created.ID)
	var after []byte
	if err := w.st.Pool.QueryRow(ctx, `SELECT packet FROM submissions WHERE id = $1`, created.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || w.status(created.ID) != "ready" {
		t.Fatal("re-running the job changed the packet")
	}

	// The submitter sees it with its link, packet and counts.
	d := decode[detail](t, w.expect(call{target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice}, http.StatusOK))
	if d.ReviewURL == nil || *d.ReviewURL != link || d.Packet == nil || d.CountedSeconds != wantCounted ||
		d.RawSeconds != 3600 || d.Minutes != p.Minutes || d.User != nil {
		t.Fatalf("detail = %+v", d)
	}
}

// A part whose proof doesn't verify fails the submission: nothing is
// shown to reviewers, and the submitter sees why.
func TestTamperedPartFails(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	body := mustJSON(t, submission.Upload{Project: "app", From: submittedDay, To: submittedDay, Program: "terra",
		Part: w.a.part(t, partOpts{project: "app", tamper: true})})
	created := decode[accepted](t, w.expect(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice,
		body: body, gz: true}, http.StatusAccepted))
	w.verify(created.ID)
	if s := w.status(created.ID); s != "failed" {
		t.Fatalf("status = %s", s)
	}
	token := strings.TrimPrefix(created.ReviewURL, webURL+"/r/")
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if r.Packet != nil || r.Submission["status"] != "failed" || r.Submission["error"] != nil {
		t.Fatalf("review of a failed submission = %+v", r)
	}
	d := decode[detail](t, w.expect(call{target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice}, http.StatusOK))
	// The reason is one plain sentence; the proof's hashes go to the log.
	if want := `The evidence from "laptop" doesn't match its signed records.`; d.Status != "failed" || d.Error != want || d.Packet != nil {
		t.Fatalf("detail = %+v, want error %q", d, want)
	}
	if logs := w.logs.String(); !strings.Contains(logs, "submission failed its check") ||
		!strings.Contains(logs, created.ID.String()) || !strings.Contains(logs, "detail=") {
		t.Fatalf("the failure's detail wasn't logged:\n%s", logs)
	}
	// A failed submission takes no more parts.
	part := mustJSON(t, submission.Part{DeviceID: w.b.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}})
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + created.ID.String() + "/parts", user: w.alice,
		body: part, gz: true}, http.StatusConflict)
}

// Another device adds its part: the whole submission is checked again,
// and a second part from the same device is refused.
func TestAddPart(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, _ := w.submit("terra")
	w.verify(created.ID)
	target := "/api/v1/submissions/" + created.ID.String() + "/parts"

	// B's part backs none of its minutes (it lists none): they stay
	// unbacked, now because the part doesn't list them.
	part := submission.Part{DeviceID: w.b.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}}
	out := decode[accepted](t, w.expect(call{method: http.MethodPost, target: target, user: w.alice, body: mustJSON(t, part), gz: true},
		http.StatusAccepted))
	if out.Status != "verifying" || out.ReviewURL != "" || w.status(created.ID) != "verifying" {
		t.Fatalf("added = %+v, status %s", out, w.status(created.ID))
	}
	w.expect(call{method: http.MethodPost, target: target, user: w.alice, body: mustJSON(t, part), gz: true}, http.StatusConflict)
	w.verify(created.ID)
	if n := w.count(`SELECT count(*) FROM submission_minutes WHERE submission_id = $1 AND device_id = $2 AND reason = $3`,
		created.ID, w.b.id, submission.ReasonNotListed); n != 5 || w.status(created.ID) != "ready" {
		t.Fatalf("B's rows not listed = %d, status %s", n, w.status(created.ID))
	}
	if n := w.count(`SELECT count(*) FROM river_job WHERE kind = 'submission_verify'`); n != 2 {
		t.Fatalf("queued jobs = %d", n)
	}
}
