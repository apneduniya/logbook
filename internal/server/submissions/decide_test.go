package submissions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (w *world) decide(user uuid.UUID, token, body string, want int) []byte {
	w.t.Helper()
	return w.expect(call{method: http.MethodPost, target: "/api/v1/review/" + token + "/decision", user: user, body: []byte(body)}, want)
}

func (w *world) counted(id uuid.UUID) int64 {
	w.t.Helper()
	return int64(w.count(`SELECT counted_seconds FROM submissions WHERE id = $1`, id))
}

// Only the program's members decide, never the submitter; each value has
// its own rules; every decision is kept and audited; the newest shows.
func TestDecisionRules(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")

	// Not before the re-check has finished.
	w.decide(w.rita, token, `{"value": "approved"}`, http.StatusConflict)
	w.verify(created.ID)
	counted := w.counted(created.ID)

	for name, tc := range map[string]struct {
		user uuid.UUID
		body string
		want int
	}{
		"submitter":                     {w.alice, `{"value": "approved"}`, http.StatusForbidden},
		"not a member":                  {w.mallory, `{"value": "approved"}`, http.StatusForbidden},
		"no session":                    {uuid.Nil, `{"value": "approved"}`, http.StatusUnauthorized},
		"unknown value":                 {w.rita, `{"value": "maybe"}`, http.StatusBadRequest},
		"approved with seconds":         {w.rita, `{"value": "approved", "approved_seconds": 10}`, http.StatusBadRequest},
		"approved with a reason":        {w.rita, `{"value": "approved", "reason": "fine"}`, http.StatusBadRequest},
		"adjusted to the counted total": {w.rita, fmt.Sprintf(`{"value": "adjusted", "approved_seconds": %d, "reason": "x"}`, counted), http.StatusBadRequest},
		"adjusted above counted":        {w.rita, fmt.Sprintf(`{"value": "adjusted", "approved_seconds": %d, "reason": "x"}`, counted+60), http.StatusBadRequest},
		"adjusted below zero":           {w.rita, `{"value": "adjusted", "approved_seconds": -1, "reason": "x"}`, http.StatusBadRequest},
		"adjusted without a reason":     {w.rita, `{"value": "adjusted", "approved_seconds": 60}`, http.StatusBadRequest},
		"adjusted with a blank reason":  {w.rita, `{"value": "adjusted", "approved_seconds": 60, "reason": "  "}`, http.StatusBadRequest},
		"adjusted with a long reason":   {w.rita, `{"value": "adjusted", "approved_seconds": 60, "reason": "` + strings.Repeat("x", 501) + `"}`, http.StatusBadRequest},
		"rejected without a reason":     {w.rita, `{"value": "rejected"}`, http.StatusBadRequest},
		"rejected with seconds":         {w.rita, `{"value": "rejected", "approved_seconds": 1, "reason": "x"}`, http.StatusBadRequest},
		"unknown token":                 {w.rita, `{"value": "approved"}`, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			tok := token
			if name == "unknown token" {
				tok = strings.Repeat("A", len(token))
			}
			rec := w.do(call{method: http.MethodPost, target: "/api/v1/review/" + tok + "/decision", user: tc.user, body: []byte(tc.body)})
			if rec.Code != tc.want {
				t.Fatalf("= %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
	if n := w.count(`SELECT count(*) FROM submission_decisions`); n != 0 {
		t.Fatalf("%d decisions stored", n)
	}

	// The owner approves, then a reviewer adjusts: both kept, newest wins.
	got := decode[map[string]map[string]any](t, w.decide(w.fiona, token, `{"value": "approved"}`, http.StatusOK))["decision"]
	if got["value"] != "approved" || got["by"] != "fiona" || got["approved_seconds"] != nil {
		t.Fatalf("approved = %v", got)
	}
	w.clock = w.clock.Add(time.Minute)
	body := fmt.Sprintf(`{"value": "adjusted", "approved_seconds": %d, "reason": "two idle stretches"}`, counted-60)
	got = decode[map[string]map[string]any](t, w.decide(w.rita, token, body, http.StatusOK))["decision"]
	if got["value"] != "adjusted" || got["by"] != "rita" || got["approved_seconds"] != float64(counted-60) || got["reason"] != "two idle stretches" {
		t.Fatalf("adjusted = %v", got)
	}
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if r.Decision == nil || (*r.Decision)["value"] != "adjusted" {
		t.Fatalf("review decision = %v", r.Decision)
	}
	if n := w.count(`SELECT count(*) FROM submission_decisions WHERE submission_id = $1`, created.ID); n != 2 {
		t.Fatalf("decisions kept = %d", n)
	}
	if a := w.audit(w.rita); len(a) != 1 || a[0].Action != "submission.decide" || !strings.Contains(a[0].Data, `"adjusted"`) {
		t.Fatalf("rita's audit = %+v", a)
	}
	if a := w.audit(w.fiona); len(a) != 1 || a[0].Action != "submission.decide" {
		t.Fatalf("fiona's audit = %+v", a)
	}
	// Raw rows now expire a year after the latest decision.
	if r.Packet.RawRowsDeletedAfter != w.clock.AddDate(1, 0, 0).Format(time.DateOnly) {
		t.Fatalf("raw rows deleted after %s", r.Packet.RawRowsDeletedAfter)
	}
	// A decided submission can't be withdrawn or take parts.
	w.expect(call{method: http.MethodDelete, target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice}, http.StatusConflict)
}

// A submitter banned on Hackatime after submitting can only be rejected;
// the decision refreshes a stale trust level first.
func TestDecisionWhenBannedNow(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	counted := w.counted(created.ID)

	// The stored level is stale; the refresh reads red.
	w.link(w.alice, 4321, "green", now.Add(-48*time.Hour))
	w.trust.set("red", false)
	w.decide(w.rita, token, `{"value": "approved"}`, http.StatusConflict)
	if w.trust.calls != 1 {
		t.Fatalf("refreshes = %d", w.trust.calls)
	}
	w.decide(w.rita, token, fmt.Sprintf(`{"value": "adjusted", "approved_seconds": %d, "reason": "x"}`, counted-1), http.StatusConflict)
	w.decide(w.rita, token, `{"value": "rejected", "reason": "banned on Hackatime"}`, http.StatusOK)

	// The review page shows the level now, masked, with when it was read.
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if r.Packet.Trust.AtSubmission != "green" || r.Packet.Trust.Now != "red" || !r.Packet.Trust.CheckedAt.Equal(now) {
		t.Fatalf("trust = %+v", r.Packet.Trust)
	}
}

// A submission without a program takes no decisions; a program's deleted
// app loses its members' right to decide.
func TestDecisionNeedsALiveProgram(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("")
	w.verify(created.ID)
	w.decide(w.fiona, token, `{"value": "approved"}`, http.StatusForbidden)

	w.exec(`DELETE FROM submissions`)
	created, token = w.submit("terra")
	w.verify(created.ID)
	if err := w.oauth.DeleteApp(t.Context(), w.fiona, w.terra); err != nil {
		t.Fatal(err)
	}
	w.decide(w.fiona, token, `{"value": "approved"}`, http.StatusForbidden)
	// The submission keeps the program's name.
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if p := r.Submission["program"].(map[string]any); p["slug"] != "terra" {
		t.Fatalf("program = %v", p)
	}
}

// A decision that echoes the version the reviewer read is refused once a
// part was added since: the packet changed under them.
func TestDecisionOnAStalePacket(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	read := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	stale, ok := read.Submission["version"].(float64)
	if !ok {
		t.Fatalf("review submission = %v", read.Submission)
	}

	part := mustJSON(t, map[string]any{"device_id": w.b.id.String(), "blocks": []any{}, "minutes": []any{}})
	w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + created.ID.String() + "/parts", user: w.alice,
		body: part, gz: true}, http.StatusAccepted)
	w.verify(created.ID)

	body := w.decide(w.rita, token, fmt.Sprintf(`{"value": "approved", "version": %d}`, int(stale)), http.StatusConflict)
	if !strings.Contains(string(body), "reload") {
		t.Fatalf("refusal = %s", body)
	}
	cur := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK)).Submission["version"].(float64)
	if cur == stale {
		t.Fatal("version unchanged by a new part")
	}
	w.decide(w.rita, token, fmt.Sprintf(`{"value": "approved", "version": %d}`, int(cur)), http.StatusOK)
}

// A submitter who was linked at submission and unlinked Hackatime since
// can't be approved: a ban can no longer be checked. The review page says
// they aren't linked now; rejecting is still allowed.
func TestDecisionAfterUnlinking(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	w.exec(`DELETE FROM hackatime_links WHERE user_id = $1`, w.alice)

	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if tr := r.Packet.Trust; !tr.HackatimeLinked || tr.HackatimeLinkedNow == nil || *tr.HackatimeLinkedNow {
		t.Fatalf("trust = %+v", tr)
	}
	body := w.decide(w.rita, token, `{"value": "approved"}`, http.StatusConflict)
	if !strings.Contains(string(body), "unlinked Hackatime") {
		t.Fatalf("refusal = %s", body)
	}
	w.decide(w.rita, token, fmt.Sprintf(`{"value": "adjusted", "approved_seconds": %d, "reason": "x"}`, w.counted(created.ID)-1),
		http.StatusConflict)
	w.decide(w.rita, token, `{"value": "rejected", "reason": "can't check Hackatime"}`, http.StatusOK)

	// Linking again restores approvals.
	w.link(w.alice, 4321, "green", now)
	w.decide(w.fiona, token, `{"value": "approved"}`, http.StatusOK)
	r = decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if tr := r.Packet.Trust; tr.HackatimeLinkedNow == nil || !*tr.HackatimeLinkedNow {
		t.Fatalf("trust after linking again = %+v", tr)
	}
}

// A submitter whose Hackatime account was banned after submitting can't
// dodge the ban by linking another account: approving or adjusting is
// refused while the link isn't to the account they submitted under, and
// the review page says the account changed. Rejecting is still allowed;
// linking the original account again restores approvals.
func TestDecisionAfterSwitchingHackatimeAccount(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	if n := w.count(`SELECT count(*) FROM submissions WHERE id = $1 AND hackatime_user_id = 4321`, created.ID); n != 1 {
		t.Fatal("the submission doesn't record the Hackatime account it was made under")
	}
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if tr := r.Packet.Trust; tr.HackatimeAccountChanged == nil || *tr.HackatimeAccountChanged {
		t.Fatalf("trust before switching = %+v", tr)
	}

	// Account 4321 is banned; within a day, alice links a green account
	// 9999 instead (allowed while the stored level isn't red yet).
	w.exec(`UPDATE hackatime_links SET hackatime_user_id = 9999, trust_level = 'green', trust_checked_at = $2
		WHERE user_id = $1`, w.alice, now)

	r = decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if tr := r.Packet.Trust; tr.HackatimeAccountChanged == nil || !*tr.HackatimeAccountChanged ||
		tr.HackatimeLinkedNow == nil || !*tr.HackatimeLinkedNow || tr.Now != "green" {
		t.Fatalf("trust after switching = %+v", tr)
	}
	body := w.decide(w.rita, token, `{"value": "approved"}`, http.StatusConflict)
	if !strings.Contains(string(body), "another Hackatime account") {
		t.Fatalf("refusal = %s", body)
	}
	w.decide(w.rita, token, fmt.Sprintf(`{"value": "adjusted", "approved_seconds": %d, "reason": "x"}`, w.counted(created.ID)-1),
		http.StatusConflict)
	w.decide(w.rita, token, `{"value": "rejected", "reason": "switched Hackatime accounts"}`, http.StatusOK)

	// Back on the original account, approvals work again.
	w.exec(`UPDATE hackatime_links SET hackatime_user_id = 4321 WHERE user_id = $1`, w.alice)
	w.decide(w.fiona, token, `{"value": "approved"}`, http.StatusOK)
	r = decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if tr := r.Packet.Trust; tr.HackatimeAccountChanged == nil || *tr.HackatimeAccountChanged {
		t.Fatalf("trust back on the original account = %+v", tr)
	}
}

// A submitter who wasn't linked at submission has no account to compare:
// linking one later doesn't block approvals, and the packet has no
// hackatime_account_changed.
func TestDecisionLinkedAfterSubmitting(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.exec(`DELETE FROM hackatime_links`)
	created, token := w.submit("terra")
	w.verify(created.ID)
	w.link(w.alice, 4321, "green", now)
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if tr := r.Packet.Trust; tr.HackatimeLinked || tr.HackatimeAccountChanged != nil {
		t.Fatalf("trust = %+v", tr)
	}
	w.decide(w.rita, token, `{"value": "approved"}`, http.StatusOK)
}
