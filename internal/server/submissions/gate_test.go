package submissions_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/submissions"
	"github.com/apneduniya/logbook/pkg/submission"
)

func (w *world) create(body []byte, want int) []byte {
	w.t.Helper()
	return w.expect(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: body, gz: true}, want)
}

// A banned Hackatime account can't submit; a stale level is re-read first,
// and when Hackatime can't be reached the last known level decides.
func TestTrustGate(t *testing.T) {
	t.Parallel()
	stale := now.Add(-25 * time.Hour)
	for _, tc := range []struct {
		name      string
		level     string
		checked   time.Time
		refreshed string // what the refresh reads; "" keeps the level
		fail      bool
		want      int
		refreshes int
	}{
		{"fresh green", "green", now.Add(-time.Hour), "", false, http.StatusAccepted, 0},
		{"fresh red", "red", now.Add(-time.Hour), "", false, http.StatusForbidden, 0},
		{"stale green read as red", "green", stale, "red", false, http.StatusForbidden, 1},
		{"stale red read as green", "red", stale, "green", false, http.StatusAccepted, 1},
		{"stale green, Hackatime down", "green", stale, "", true, http.StatusAccepted, 1},
		{"stale red, Hackatime down", "red", stale, "", true, http.StatusForbidden, 1},
		{"yellow is not banned", "yellow", now.Add(-time.Hour), "", false, http.StatusAccepted, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.link(w.alice, 4321, tc.level, tc.checked)
			w.trust.set(tc.refreshed, tc.fail)
			body := w.create(w.upload("terra"), tc.want)
			if w.trust.calls != tc.refreshes {
				t.Fatalf("refreshes = %d, want %d", w.trust.calls, tc.refreshes)
			}
			if tc.want == http.StatusForbidden {
				if !strings.Contains(string(body), "banned") || w.count(`SELECT count(*) FROM submissions`) != 0 {
					t.Fatalf("banned = %s", body)
				}
			}
		})
	}
}

// A user with no Hackatime link can submit; the packet says so.
func TestUnlinkedCanSubmit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.exec(`DELETE FROM hackatime_links`)
	created, token := w.submit("terra")
	w.verify(created.ID)
	r := decode[review](t, w.expect(call{target: "/api/v1/review/" + token}, http.StatusOK))
	if r.Packet.Trust.HackatimeLinked || r.Packet.Trust.AtSubmission != "blue" || r.Packet.Trust.Now != "blue" ||
		r.Packet.Trust.CheckedAt != nil || r.Submission["user"].(map[string]any)["hackatime_id"] != nil {
		t.Fatalf("trust = %+v, user %v", r.Packet.Trust, r.Submission["user"])
	}
	if !strings.Contains(r.Packet.Justification, "re-computed") {
		t.Fatalf("justification = %q", r.Packet.Justification)
	}
}

// Upload shape and state checks answer before anything is stored.
func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.program(w.fiona, "private", false)
	good := func(edit func(*submission.Upload)) []byte {
		u := submission.Upload{Project: "app", From: submittedDay, To: submittedDay, Program: "terra",
			Part: w.a.part(t, partOpts{project: "app"})}
		edit(&u)
		return mustJSON(t, u)
	}
	for _, tc := range []struct {
		name string
		body []byte
		want int
	}{
		{"not JSON", []byte("{"), http.StatusBadRequest},
		{"no project", good(func(u *submission.Upload) { u.Project = "  " }), http.StatusBadRequest},
		{"bad range", good(func(u *submission.Upload) { u.From = "2026-11-21" }), http.StatusBadRequest},
		{"over 92 days", good(func(u *submission.Upload) { u.From = "2026-08-01" }), http.StatusBadRequest},
		{"range ends after a device's sealed day", good(func(u *submission.Upload) { u.To = "2026-11-21" }), http.StatusBadRequest},
		{"unknown device", good(func(u *submission.Upload) { u.Part.DeviceID = "00000000-0000-4000-8000-000000000000" }), http.StatusBadRequest},
		{"block with no stored record", good(func(u *submission.Upload) { u.Part.Blocks[0].Day = "2026-11-19" }), http.StatusBadRequest},
		{"unknown program", good(func(u *submission.Upload) { u.Program = "nope" }), http.StatusNotFound},
		{"program that isn't public", good(func(u *submission.Upload) { u.Program = "private" }), http.StatusNotFound},
		{"nothing counted in the range", good(func(u *submission.Upload) {
			u.From, u.To = "2026-11-19", "2026-11-19"
			u.Part.Blocks = nil
			u.Part.Minutes = nil
		}), http.StatusConflict},
		{"another project with no rows", good(func(u *submission.Upload) {
			u.Project = "zzz"
			u.Part.Minutes = nil
		}), http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.resetLimits()
			rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: tc.body, gz: true})
			if rec.Code != tc.want {
				t.Fatalf("= %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
	if n := w.count(`SELECT count(*) FROM submissions`); n != 0 {
		t.Fatalf("%d submissions stored", n)
	}
}

// The body must be gzip, at most 32 MiB as sent and 256 MiB decompressed
// (smaller limits here), checked while it streams.
func TestBodyLimits(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(o *submissions.Options) { o.MaxCompressedBytes, o.MaxDecompressedBytes = 64<<10, 2<<20 })
	body := w.upload("terra")

	// Not gzip.
	rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: body})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("plain body = %d %s", rec.Code, rec.Body)
	}
	rec = w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: []byte("not gzip")})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("no encoding = %d", rec.Code)
	}
	// Says gzip but isn't, or is cut short.
	for _, b := range [][]byte{[]byte("plain text"), gz(t, body)[:40]} {
		rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: b, encoding: "gzip"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("broken gzip = %d %s", rec.Code, rec.Body)
		}
	}
	// Too large decompressed: highly compressible padding.
	padded := append([]byte(`{"project":"app","pad":"`), bytes.Repeat([]byte("a"), 3<<20)...)
	padded = append(padded, []byte(`"}`)...)
	if rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: padded, gz: true}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("decompressed too large = %d %s", rec.Code, rec.Body)
	}
	// Too large compressed: random text in a JSON string barely compresses.
	random := make([]byte, 150<<10)
	_, _ = rand.Read(random)
	noise := []byte(`{"project":"app","pad":"` + base64.StdEncoding.EncodeToString(random) + `"}`)
	if rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: noise, gz: true}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("compressed too large = %d %s", rec.Code, rec.Body)
	}
	// Trailing data after the JSON value.
	if rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: append(body, []byte(" {}")...), gz: true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d %s", rec.Code, rec.Body)
	}
	// Within the limits it is accepted.
	w.create(body, http.StatusAccepted)
	if n := w.count(`SELECT count(*) FROM submissions`); n != 1 {
		t.Fatalf("%d submissions stored", n)
	}
}

// Uploads share one budget per user: a burst of 10.
func TestUploadRateLimit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i := range 10 {
		rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice, body: []byte("{"), gz: true})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("upload %d = %d", i, rec.Code)
		}
	}
	rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions/" + w.a.id.String() + "/parts", user: w.alice, body: []byte("{"), gz: true})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("11th upload = %d", rec.Code)
	}
	// Another user has their own budget.
	if rec := w.do(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.bob, body: []byte("{"), gz: true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bob = %d", rec.Code)
	}
}
