package submissions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/submission"
)

// lateRow is a row of B's that reaches the server after alice's
// submission froze its rows: app at 09:50, with a key whose window holds it.
func (w *world) lateRow() submission.PartMinute {
	w.t.Helper()
	m := at(9, 50)
	tt, h := m.Truncate(10*time.Minute).Add(40*time.Minute), int64(5)
	w.credit(w.alice, minuteRow(w.b.id, m, "app", 60, &tt, &h))
	return submission.PartMinute{Minute: m, ComputedAt: tt, Snapshot: h}
}

// A part that lists a minute the submission didn't freeze (B's row that
// arrived after the freeze) is refused when it is uploaded, so it never
// reaches the re-check, which would fail the whole submission and drop A's
// verified minutes.
func TestAddPartListingUnfrozenRowIsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("terra")
	w.verify(created.ID)
	late := w.lateRow()
	target := "/api/v1/submissions/" + created.ID.String() + "/parts"

	part := submission.Part{DeviceID: w.b.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{late}}
	body := w.expect(call{method: http.MethodPost, target: target, user: w.alice, body: mustJSON(t, part), gz: true},
		http.StatusBadRequest)
	if !strings.Contains(string(body), "isn't a frozen row") || strings.Contains(string(body), "malformed submission") {
		t.Fatalf("refusal = %s", body)
	}
	// One of B's frozen minutes at another key is refused too.
	frozen := at(9, 40)
	other := submission.PartMinute{Minute: frozen, ComputedAt: frozen.Add(30 * time.Minute), Snapshot: 6}
	part.Minutes = []submission.PartMinute{other}
	w.expect(call{method: http.MethodPost, target: target, user: w.alice, body: mustJSON(t, part), gz: true},
		http.StatusBadRequest)

	if s := w.status(created.ID); s != "ready" {
		t.Fatalf("status = %s", s)
	}
	if n := w.count(`SELECT count(*) FROM submission_parts WHERE submission_id = $1`, created.ID); n != 1 {
		t.Fatalf("parts = %d", n)
	}
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if r.Packet == nil || r.Packet.Minutes.Verified == 0 {
		t.Fatalf("packet after a refused part = %+v", r.Packet)
	}
}

// The plan of a submission is its frozen rows: a part built from it lists
// only those, so adding it can't fail the submission. Its project and
// range must be the submission's, and it is only the submitter's.
func TestSubmissionPlan(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, _ := w.submit("terra")
	w.verify(created.ID)
	w.lateRow()

	base := "/api/v1/me/submissions/plan?project=app&from=" + submittedDay + "&to=" + submittedDay + "&device=" + w.b.id.String()
	live := decode[submission.Plan](t, w.expect(call{target: base, user: w.alice}, http.StatusOK))
	frozen := decode[submission.Plan](t, w.expect(call{target: base + "&submission=" + created.ID.String(), user: w.alice}, http.StatusOK))
	if len(live.Minutes) != len(frozen.Minutes)+1 {
		t.Fatalf("live plan %d minutes, frozen plan %d", len(live.Minutes), len(frozen.Minutes))
	}
	want := w.count(`SELECT count(*) FROM submission_minutes WHERE submission_id = $1`, created.ID)
	if len(frozen.Minutes) != want || len(frozen.Devices) != 2 {
		t.Fatalf("frozen plan = %d minutes (want %d), devices %+v", len(frozen.Minutes), want, frozen.Devices)
	}
	for _, m := range frozen.Minutes {
		if m.DeviceID == w.b.id.String() && m.Minute.Equal(at(9, 50)) {
			t.Fatal("the frozen plan has a row that arrived after the freeze")
		}
	}
	for _, d := range frozen.Devices {
		if d.ThisDevice != (d.ID == w.b.id.String()) {
			t.Fatalf("device = %+v", d)
		}
	}

	// Another range or project says which the submission has.
	body := w.expect(call{target: "/api/v1/me/submissions/plan?project=app&from=2026-11-19&to=" + submittedDay +
		"&submission=" + created.ID.String(), user: w.alice}, http.StatusBadRequest)
	if !strings.Contains(string(body), fmt.Sprintf(`\"app\" from %s to %s`, submittedDay, submittedDay)) {
		t.Fatalf("wrong range = %s", body)
	}
	w.expect(call{target: "/api/v1/me/submissions/plan?project=lib&from=" + submittedDay + "&to=" + submittedDay +
		"&submission=" + created.ID.String(), user: w.alice}, http.StatusBadRequest)
	// Not bob's, and not a submission id.
	w.expect(call{target: base + "&submission=" + created.ID.String(), user: w.bob}, http.StatusNotFound)
	w.expect(call{target: base + "&submission=nope", user: w.alice}, http.StatusNotFound)
	// A withdrawn submission takes no parts, so it has no plan.
	w.expect(call{method: http.MethodDelete, target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice}, http.StatusNoContent)
	w.expect(call{target: base + "&submission=" + created.ID.String(), user: w.alice}, http.StatusConflict)
}

// Once retention has deleted a submission's stored parts, it takes no new
// part: the re-check of the whole submission would find them gone and
// fail it, clearing the packet the program already has.
func TestAddPartAfterRawRowsDeleted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	created, token := w.submit("") // no program: it is never decided
	w.verify(created.ID)
	w.clock = now.AddDate(1, 0, 1)
	if n, err := w.checker.ExpireRaw(t.Context()); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	part := submission.Part{DeviceID: w.b.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}}
	body := w.expect(call{method: http.MethodPost, target: "/api/v1/submissions/" + created.ID.String() + "/parts", user: w.alice,
		body: mustJSON(t, part), gz: true}, http.StatusConflict)
	if !strings.Contains(string(body), "deleted") {
		t.Fatalf("refusal = %s", body)
	}
	if s := w.status(created.ID); s != "ready" {
		t.Fatalf("status = %s", s)
	}
	if r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK)); r.Packet == nil {
		t.Fatal("packet cleared")
	}
}

// A revoked device can't seal more days, so it doesn't hold a range back:
// its rows there stay unbacked. An unrevoked device that hasn't sealed
// the range holds it back, and the refusal says revoking it would help.
func TestRevokedDeviceDoesNotHoldTheRangeBack(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	old := w.register(w.alice, "old-laptop", 3)
	m := at(9, 55)
	tt, h := m.Truncate(10*time.Minute).Add(40*time.Minute), int64(5)
	w.credit(w.alice, minuteRow(old.id, m, "app", 60, &tt, &h))

	target := "/api/v1/me/submissions/plan?project=app&from=" + submittedDay + "&to=" + submittedDay
	body := w.expect(call{target: target, user: w.alice}, http.StatusBadRequest)
	if !strings.Contains(string(body), `\"old-laptop\" has sealed no evidence yet`) || !strings.Contains(string(body), "revoke it") {
		t.Fatalf("refusal = %s", body)
	}
	w.create(w.upload("terra"), http.StatusBadRequest)

	w.exec(`UPDATE devices SET revoked_at = $2 WHERE id = $1`, old.id, now)
	p := decode[submission.Plan](t, w.expect(call{target: target, user: w.alice}, http.StatusOK))
	if len(p.Devices) != 3 {
		t.Fatalf("devices = %+v", p.Devices)
	}
	for _, d := range p.Devices {
		if d.Revoked != (d.ID == old.id.String()) {
			t.Fatalf("device = %+v", d)
		}
	}
	created := decode[accepted](t, w.create(w.upload("terra"), http.StatusAccepted))
	w.verify(created.ID)
	if n := w.count(`SELECT count(*) FROM submission_minutes WHERE submission_id = $1 AND device_id = $2 AND result = 'unbacked'`,
		created.ID, old.id); n != 1 || w.status(created.ID) != "ready" {
		t.Fatalf("old device's unbacked rows = %d, status %s", n, w.status(created.ID))
	}
}
