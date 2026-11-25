package admin_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// user/devices lists every device with its chain: days sealed, first and
// last, the gap between, and refused conflicting uploads.
func TestUserDevices(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	exec(t, f.st, `INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id, data)
		VALUES ($1, 'evidence_conflict', 'device', $2, '{"day": "2026-06-10", "stored": {"seq_to": 9}, "uploaded": {"seq_to": 7}}')`,
		f.orpheus, f.laptop.String())
	exec(t, f.st, `UPDATE devices SET revoked_at = now() WHERE id = $1`, f.laptop)
	type device struct {
		ID        uuid.UUID  `json:"id"`
		Name      string     `json:"name"`
		RevokedAt *time.Time `json:"revoked_at"`
		PublicKey string     `json:"public_key"`
		Chain     struct {
			DaysSealed     int     `json:"days_sealed"`
			DaysWithBlock  int     `json:"days_with_block"`
			FirstDay       *string `json:"first_day"`
			LastDay        *string `json:"last_day"`
			GapDays        int     `json:"gap_days"`
			Gaps           []struct{ From, To string }
			RefusedUploads []struct {
				Day    string         `json:"day"`
				Detail map[string]any `json:"detail"`
			} `json:"refused_uploads"`
		} `json:"chain"`
	}
	out := decode[struct {
		UserID  int64    `json:"user_id"`
		Devices []device `json:"devices"`
	}](t, f.expect(t, call{target: "/api/admin/v1/user/devices?id=" + strconv.FormatInt(f.orpheusID, 10)}, http.StatusOK))
	if out.UserID != f.orpheusID || len(out.Devices) != 1 {
		t.Fatalf("devices = %+v", out)
	}
	d := out.Devices[0]
	c := d.Chain
	if d.ID != f.laptop || d.Name != "laptop" || d.RevokedAt == nil || d.PublicKey == "" ||
		c.DaysSealed != 2 || c.DaysWithBlock != 0 || *c.FirstDay != "2026-06-08" || *c.LastDay != "2026-06-10" ||
		c.GapDays != 1 || len(c.Gaps) != 1 || c.Gaps[0].From != "2026-06-09" || c.Gaps[0].To != "2026-06-09" ||
		len(c.RefusedUploads) != 1 || c.RefusedUploads[0].Day != "2026-06-10" || c.RefusedUploads[0].Detail["stored"] == nil {
		t.Fatalf("device = %+v", d)
	}
	pat := decode[struct {
		Devices []any `json:"devices"`
	}](t, f.expect(t, call{target: "/api/admin/v1/user/devices?id=" + strconv.FormatInt(f.patID, 10)}, http.StatusOK))
	if pat.Devices == nil || len(pat.Devices) != 0 {
		t.Fatalf("pat's devices = %v", pat)
	}
}

// user/minutes pages through every credited row, with filters.
func TestUserMinutes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := strconv.FormatInt(f.orpheusID, 10)
	type page struct {
		Minutes []struct {
			DeviceID uuid.UUID `json:"device_id"`
			Minute   time.Time `json:"minute"`
			Project  string    `json:"project"`
			Label    string    `json:"label"`
			Credited int       `json:"credited"`
			Cuts     []string  `json:"cuts"`
			Key      *struct{} `json:"key"`
		} `json:"minutes"`
		Limit   int  `json:"limit"`
		Offset  int  `json:"offset"`
		HasMore bool `json:"has_more"`
	}
	get := func(q string) page {
		return decode[page](t, f.expect(t, call{target: "/api/admin/v1/user/minutes?id=" + id + q}, http.StatusOK))
	}
	all := get("")
	if len(all.Minutes) != 25 || all.HasMore || all.Limit != 1000 {
		t.Fatalf("all = %d minutes, has_more %v, limit %d", len(all.Minutes), all.HasMore, all.Limit)
	}
	m := all.Minutes[0]
	if m.DeviceID != f.laptop || !m.Minute.Equal(day(9, 0)) || m.Project != "logbook" || m.Label != "you" ||
		m.Credited != 60 || m.Cuts == nil || m.Key != nil {
		t.Fatalf("first minute = %+v", m)
	}
	p := get("&limit=10&offset=20")
	if len(p.Minutes) != 5 || p.HasMore || p.Offset != 20 {
		t.Fatalf("last page = %+v", p)
	}
	if p := get("&limit=10"); len(p.Minutes) != 10 || !p.HasMore {
		t.Fatalf("first page = %d, has_more %v", len(p.Minutes), p.HasMore)
	}
	if p := get("&project=secret"); len(p.Minutes) != 5 || p.Minutes[0].Project != "secret" {
		t.Fatalf("secret = %+v", p)
	}
	if p := get("&start_date=" + strconv.FormatInt(day(9, 15).Unix(), 10) + "&end_date=2026-06-10"); len(p.Minutes) != 10 {
		t.Fatalf("09:15 to the end of the day = %d minutes", len(p.Minutes))
	}
	f.expect(t, call{target: "/api/admin/v1/user/minutes?id=" + id + "&end_date=soon"}, http.StatusUnprocessableEntity)
}

// user/audit_log pages through entries where the user acts or is the
// subject (their devices included), newest first.
func TestUserAuditLog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	exec(t, f.st, `INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id) VALUES ($1, 'api_key.create', 'api_key', 'k1')`, f.orpheus)
	exec(t, f.st, `INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id) VALUES (NULL, 'user.username_reserved', 'user', $1)`, f.orpheus.String())
	exec(t, f.st, `INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id) VALUES ($1, 'device.register', 'device', $2)`, f.orpheus, f.laptop.String())
	exec(t, f.st, `INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id) VALUES ($1, 'auth.login', 'user', $2)`, f.pat, f.pat.String())
	type page struct {
		Entries []struct {
			ID     int64  `json:"id"`
			Action string `json:"action"`
			Actor  *struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"actor"`
		} `json:"entries"`
		HasMore bool `json:"has_more"`
	}
	target := "/api/admin/v1/user/audit_log?id=" + strconv.FormatInt(f.orpheusID, 10)
	p := decode[page](t, f.expect(t, call{target: target}, http.StatusOK))
	var actions []string
	for _, e := range p.Entries {
		actions = append(actions, e.Action)
	}
	if len(actions) != 3 || actions[0] != "device.register" || actions[1] != "user.username_reserved" || actions[2] != "api_key.create" ||
		p.Entries[1].Actor != nil || p.Entries[0].Actor == nil || p.Entries[0].Actor.Username != "orpheus" {
		t.Fatalf("entries = %v %+v", actions, p.Entries)
	}
	// The read itself is now an entry about orpheus.
	p = decode[page](t, f.expect(t, call{target: target + "&limit=2"}, http.StatusOK))
	if len(p.Entries) != 2 || !p.HasMore || p.Entries[0].Action != "admin.read" || p.Entries[0].Actor.Username != "fiona" {
		t.Fatalf("page = %+v", p)
	}
}

// Submissions that don't exist, or ids that aren't UUIDs, are 404 with
// the submissions API's message.
func TestSubmissionNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, target := range []string{
		"/api/admin/v1/submissions/nope", "/api/admin/v1/submissions/" + uuid.NewString(),
		"/api/admin/v1/submissions/" + uuid.NewString() + "/evidence", "/api/admin/v1/submissions/" + uuid.NewString() + "/bundle",
	} {
		if body := decode[map[string]string](t, f.expect(t, call{target: target}, http.StatusNotFound)); body["error"] != "submission not found" {
			t.Fatalf("%s: %v", target, body)
		}
	}
	subs := decode[struct {
		Submissions []any `json:"submissions"`
	}](t, f.expect(t, call{target: "/api/admin/v1/user/submissions?id=" + strconv.FormatInt(f.orpheusID, 10)}, http.StatusOK))
	if subs.Submissions == nil || len(subs.Submissions) != 0 {
		t.Fatalf("submissions = %v", subs)
	}
}
