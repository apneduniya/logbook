package serverapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status int
		body   string
		retry  string
		want   Kind
		wait   time.Duration
	}{
		{"unauthorized", 401, `{"error":"unauthorized"}`, "", KindAuth, 0},
		{"unauthorized html", 401, `<html>`, "", KindAuth, 0},
		{"revoked", 403, `{"error":"device revoked"}`, "", KindRevoked, 0},
		{"filter 403", 403, `<html>Blocked by your school</html>`, "", KindRetry, 0},
		{"unknown device", 404, `{"error":"not found"}`, "", KindUnknownDevice, 0},
		{"proxy 404", 404, `Not Found`, "", KindRetry, 0},
		{"conflict", 409, `{"error":"chain mismatch"}`, "", KindConflict, 0},
		{"proxy 409", 409, `<html>Conflict</html>`, "", KindRetry, 0},
		{"invalid", 400, `{"error":"bad signature"}`, "", KindInvalid, 0},
		{"too large", 413, `{"error":"too many minutes"}`, "", KindInvalid, 0},
		{"proxy 400", 400, `<html>`, "", KindRetry, 0},
		{"rate limited", 429, `{"error":"slow down"}`, "30", KindRetry, 30 * time.Second},
		{"server error", 503, ``, now.Add(time.Minute).Format(http.TimeFormat), KindRetry, time.Minute},
		{"redirect", 302, ``, "", KindRetry, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := classify(tt.status, tt.retry, []byte(tt.body), now)
			if e.Kind != tt.want || e.RetryAfter != tt.wait {
				t.Fatalf("classify = kind %d wait %v, want %d %v", e.Kind, e.RetryAfter, tt.want, tt.wait)
			}
			if KindOf(e) != tt.want || e.Error() == "" {
				t.Fatalf("KindOf = %d, Error() = %q", KindOf(e), e.Error())
			}
		})
	}
	if KindOf(errors.New("x")) != KindRetry {
		t.Fatal("plain errors must be retried")
	}
}

func TestErrorPredicates(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		status          int
		body            string
		keyTaken, large bool
	}{
		{"key taken", 409, `{"error":"public key is registered to another account"}`, true, false},
		{"device limit", 409, `{"error":"you already have 20 active devices; revoke one first"}`, false, false},
		{"proxy 409", 409, `public key is registered to another account`, false, false},
		{"413", 413, `{"error":"request body too large"}`, false, true},
		{"too many minutes", 400, `{"error":"at most 1000 minutes per request"}`, false, true},
		{"unknown rules", 400, `{"error":"unknown rules_version"}`, false, false},
		{"proxy 413", 413, `<html>`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := classify(tt.status, "", []byte(tt.body), now)
			if IsKeyTaken(e) != tt.keyTaken || IsTooLarge(e) != tt.large {
				t.Fatalf("IsKeyTaken = %v, IsTooLarge = %v; want %v, %v", IsKeyTaken(e), IsTooLarge(e), tt.keyTaken, tt.large)
			}
		})
	}
	if IsKeyTaken(errors.New("x")) || IsTooLarge(nil) {
		t.Fatal("plain errors matched")
	}
}

type recorded struct {
	method, path, auth, ua string
	body                   map[string]any
}

func server(t *testing.T, status int, resp string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path, rec.auth, rec.ua = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.UserAgent()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &rec.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestRegisterDevice(t *testing.T) {
	const id = "0B7C2F1E-3A4D-4E5F-8A9B-0C1D2E3F4A5B"
	srv, rec := server(t, 201, `{"id":"`+id+`"}`)
	c := New(srv.Client(), "logbook-agent/test")
	key := make([]byte, 32)
	key[0] = 7
	got, err := c.RegisterDevice(t.Context(), Target{BaseURL: srv.URL + "/", APIKey: "k"},
		DeviceRequest{Name: "laptop", OS: "linux", PublicKey: key})
	if err != nil || got != strings.ToLower(id) {
		t.Fatalf("RegisterDevice = %q, %v", got, err)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/devices" || rec.auth != "Bearer k" || rec.ua != "logbook-agent/test" {
		t.Fatalf("request = %+v", rec)
	}
	if rec.body["public_key"] != base64.StdEncoding.EncodeToString(key) || rec.body["name"] != "laptop" || rec.body["os"] != "linux" {
		t.Fatalf("body = %v", rec.body)
	}

	for _, resp := range []string{`{"id":"nope"}`, `<html>`, `{}`} {
		srv, _ := server(t, 200, resp)
		if _, err := New(srv.Client(), "ua").RegisterDevice(t.Context(), Target{BaseURL: srv.URL}, DeviceRequest{}); KindOf(err) != KindRetry || err == nil {
			t.Fatalf("response %s: err = %v", resp, err)
		}
	}
}

func TestUploadMinutes(t *testing.T) {
	computed := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	req := MinutesRequest{RulesVersion: "v1", Minutes: []Minute{
		{Minute: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), Project: "app", Label: "you", Credited: 60, Cuts: []string{}, Flags: []string{},
			ComputedAt: computed, Snapshot: 1234},
		{Minute: time.Date(2026, 11, 20, 10, 1, 0, 0, time.UTC), Project: "app", Label: "idle", Cuts: []string{"idle"}, Flags: []string{},
			ComputedAt: computed, Snapshot: 1234},
	}}
	srv, rec := server(t, 200, `{"accepted":1,"rejected":[{"index":1,"error":"too old"}]}`)
	res, err := New(srv.Client(), "ua").UploadMinutes(t.Context(), Target{BaseURL: srv.URL, APIKey: "k"}, "dev/1", req)
	if err != nil || res.Accepted != 1 || len(res.Rejected) != 1 || res.Rejected[0].Index != 1 || res.Rejected[0].Stored != nil {
		t.Fatalf("UploadMinutes = %+v, %v", res, err)
	}
	if rec.path != "/api/v1/devices/dev%2F1/minutes" && rec.path != "/api/v1/devices/dev/1/minutes" {
		t.Fatalf("path = %s", rec.path)
	}
	ms := rec.body["minutes"].([]any)
	first := ms[0].(map[string]any)
	if first["minute"] != "2026-11-20T10:00:00Z" || rec.body["rules_version"] != "v1" ||
		first["computed_at"] != "2026-11-20T10:30:00Z" || first["snapshot"] != float64(1234) {
		t.Fatalf("body = %v", rec.body)
	}

	for _, resp := range []string{`{"accepted":2,"rejected":[{"index":0,"error":"x"}]}`, `{"accepted":1,"rejected":[{"index":5,"error":"x"}]}`, `ok`} {
		srv, _ := server(t, 200, resp)
		if _, err := New(srv.Client(), "ua").UploadMinutes(t.Context(), Target{BaseURL: srv.URL}, "d", req); KindOf(err) != KindRetry || err == nil {
			t.Fatalf("response %s: err = %v", resp, err)
		}
	}
}

// A refusal because the minute is final carries the rows the server holds,
// keyed or (stored before keys) not; an empty list means it holds none.
func TestUploadMinutesStoredRows(t *testing.T) {
	req := MinutesRequest{RulesVersion: "v1", Minutes: []Minute{{Project: "a"}, {Project: "b"}, {Project: "c"}}}
	srv, _ := server(t, 200, `{"accepted":0,"rejected":[
		{"index":0,"error":"minute is final","stored":[
			{"project":"a","label":"you","credited":45,"cuts":[],"flags":["low_signal"],"computed_at":"2026-11-20T15:59:00+05:30","snapshot":77},
			{"project":"z","label":"idle","credited":0,"cuts":["idle"],"flags":[],"computed_at":null,"snapshot":null}]},
		{"index":1,"error":"minute is final","stored":[]},
		{"index":2,"error":"project refused"}]}`)
	res, err := New(srv.Client(), "ua").UploadMinutes(t.Context(), Target{BaseURL: srv.URL}, "d", req)
	if err != nil || len(res.Rejected) != 3 {
		t.Fatalf("UploadMinutes = %+v, %v", res, err)
	}
	st := res.Rejected[0].Stored
	if len(st) != 2 || st[0].Credited != 45 || st[0].Snapshot == nil || *st[0].Snapshot != 77 || st[0].ComputedAt == nil ||
		!st[0].ComputedAt.Equal(time.Date(2026, 11, 20, 10, 29, 0, 0, time.UTC)) || st[0].Flags[0] != "low_signal" {
		t.Fatalf("stored rows = %+v", st)
	}
	if st[1].ComputedAt != nil || st[1].Snapshot != nil || st[1].Cuts[0] != "idle" {
		t.Fatalf("unkeyed stored row = %+v", st[1])
	}
	if res.Rejected[1].Stored == nil || len(res.Rejected[1].Stored) != 0 {
		t.Fatalf("empty stored list = %#v, want empty and not nil", res.Rejected[1].Stored)
	}
	if res.Rejected[2].Stored != nil {
		t.Fatalf("a refusal without stored rows has %+v", res.Rejected[2].Stored)
	}
}

// The server's time comes from the body's server_time, never from the Date
// header (a proxy may drop or rewrite it). A missing or malformed value is
// zero and doesn't fail the upload.
func TestUploadMinutesServerTime(t *testing.T) {
	tests := []struct {
		name, body string
		want       time.Time
	}{
		{"utc", `{"accepted":0,"rejected":[],"server_time":"2026-11-20T12:00:30.25Z"}`,
			time.Date(2026, 11, 20, 12, 0, 30, 250e6, time.UTC)},
		{"offset", `{"accepted":0,"rejected":[],"server_time":"2026-11-20T17:30:30+05:30"}`,
			time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)},
		{"missing", `{"accepted":0,"rejected":[]}`, time.Time{}},
		{"malformed", `{"accepted":0,"rejected":[],"server_time":"yesterday"}`, time.Time{}},
		{"wrong type", `{"accepted":0,"rejected":[],"server_time":17}`, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Date", "Wed, 25 Nov 2026 12:00:00 GMT")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			res, err := New(srv.Client(), "ua").UploadMinutes(t.Context(), Target{BaseURL: srv.URL}, "d",
				MinutesRequest{RulesVersion: "v1", Minutes: []Minute{}})
			if err != nil || !res.ServerTime.Equal(tt.want) || res.ServerTime.Location() != time.UTC {
				t.Fatalf("UploadMinutes = %+v, %v; want server time %v", res, err, tt.want)
			}
		})
	}
}

func TestUploadEvidenceDay(t *testing.T) {
	srv, rec := server(t, 409, `{"error":"chain mismatch"}`)
	err := New(srv.Client(), "ua").UploadEvidenceDay(t.Context(), Target{BaseURL: srv.URL, APIKey: "k"}, "d",
		EvidenceDayRequest{Day: "2026-11-19", Signature: []byte{1, 2, 3}, EventCounts: map[string]int{"ai": 1}})
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConflict || e.Message != "chain mismatch" {
		t.Fatalf("err = %v", err)
	}
	if rec.body["signature"] != "AQID" || rec.body["prev_chain_hash"] != "" {
		t.Fatalf("body = %v", rec.body)
	}
	// The block's signed fields (ADR 0019): a span in RFC 3339 UTC, or
	// null for a block with only its repo bucket.
	if v, ok := rec.body["time_from"]; !ok || v != nil || rec.body["seq_from"] != 0.0 || rec.body["buckets"] != 0.0 {
		t.Fatalf("body = %v, want time_from null", rec.body)
	}
	// A time in another zone is sent as the same instant in UTC.
	ist := time.FixedZone("IST", 5*3600+1800)
	from, to := time.Date(2026, 11, 19, 14, 30, 0, 0, ist), time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	srv, rec = server(t, 201, `{}`)
	err = New(srv.Client(), "ua").UploadEvidenceDay(t.Context(), Target{BaseURL: srv.URL, APIKey: "k"}, "d",
		EvidenceDayRequest{Day: "2026-11-19", SeqFrom: 12, SeqTo: 40, Buckets: 3, TimeFrom: &from, TimeTo: &to})
	if err != nil || rec.body["seq_from"] != 12.0 || rec.body["seq_to"] != 40.0 || rec.body["buckets"] != 3.0 ||
		rec.body["time_from"] != "2026-11-19T09:00:00Z" || rec.body["time_to"] != "2026-11-19T10:00:00Z" {
		t.Fatalf("body = %v, %v", rec.body, err)
	}
	srv, _ = server(t, 200, ``)
	if err := New(srv.Client(), "ua").UploadEvidenceDay(t.Context(), Target{BaseURL: srv.URL}, "d", EvidenceDayRequest{}); err != nil {
		t.Fatalf("identical re-upload err = %v", err)
	}
}

func TestNetworkErrorIsRetried(t *testing.T) {
	srv, _ := server(t, 200, `{}`)
	srv.Close()
	err := New(http.DefaultClient, "ua").UploadEvidenceDay(t.Context(), Target{BaseURL: srv.URL, APIKey: "secret-key"}, "d", EvidenceDayRequest{})
	if KindOf(err) != KindRetry || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("err = %v", err)
	}
}
