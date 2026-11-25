package serverapi

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seen is one request as the test server saw it.
type seen struct {
	method, path, query, auth, encoding, ctype string
	body                                       string
}

func submitServer(t *testing.T, status int, resp string, header map[string]string) (*httptest.Server, *seen) {
	t.Helper()
	rec := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path, rec.query = r.Method, r.URL.Path, r.URL.RawQuery
		rec.auth, rec.encoding, rec.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type")
		var body io.Reader = r.Body
		if rec.encoding == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("body isn't gzip: %v", err)
				return
			}
			body = zr
		}
		b, _ := io.ReadAll(body)
		rec.body = string(b)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestPlan(t *testing.T) {
	srv, rec := submitServer(t, 200, `{"project":"terra bot","from":"2026-11-01","to":"2026-11-20",
		"devices":[{"id":"a0000000-0000-4000-8000-000000000001","name":"laptop","sealed_through":"2026-11-24","this_device":true},
		           {"id":"b0000000-0000-4000-8000-000000000002","name":"desk","sealed_through":null,"this_device":false}],
		"minutes":[{"device_id":"a0000000-0000-4000-8000-000000000001","minute":"2026-11-03T14:21:00Z","project":"terra bot",
		  "label":"you","credited":60,"cuts":[],"flags":[],"rules_version":"v1","computed_at":"2026-11-03T14:30:00Z","snapshot":48213},
		  {"device_id":"a0000000-0000-4000-8000-000000000001","minute":"2026-11-03T14:22:00Z","project":"terra bot",
		  "label":"you","credited":60,"cuts":[],"flags":[],"rules_version":"v1","computed_at":null,"snapshot":null}]}`, nil)
	q := PlanQuery{Project: "terra bot", From: "2026-11-01", To: "2026-11-20", DeviceID: "a0000000-0000-4000-8000-000000000001"}
	plan, err := New(srv.Client(), "ua").Plan(t.Context(), Target{BaseURL: srv.URL, APIKey: "k"}, q)
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/api/v1/me/submissions/plan" || rec.auth != "Bearer k" ||
		rec.query != "device=a0000000-0000-4000-8000-000000000001&from=2026-11-01&project=terra+bot&to=2026-11-20" {
		t.Fatalf("request = %+v", rec)
	}
	if len(plan.Devices) != 2 || plan.Devices[1].SealedThrough != nil || !plan.Devices[0].ThisDevice || len(plan.Minutes) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if k, ok := plan.Minutes[0].Key(); !ok || k.H != 48213 {
		t.Fatalf("key = %+v, %v", k, ok)
	}
	if _, ok := plan.Minutes[1].Key(); ok {
		t.Fatal("a row with no key has one")
	}

	// A plan for something else is not this plan.
	q.To = "2026-11-21"
	if _, err := New(srv.Client(), "ua").Plan(t.Context(), Target{BaseURL: srv.URL}, q); err == nil {
		t.Fatal("a plan for another range was accepted")
	}
}

func TestSubmitSendsGzip(t *testing.T) {
	srv, rec := submitServer(t, 202, `{"id":"c0000000-0000-4000-8000-000000000003","status":"verifying","review_url":"https://x/r/t"}`, nil)
	c := New(srv.Client(), "ua")
	body := `{"project":"app","from":"2026-11-20","to":"2026-11-20","part":{}}`
	res, err := c.Submit(t.Context(), Target{BaseURL: srv.URL, APIKey: "k"}, []byte(body))
	if err != nil || res.ID != "c0000000-0000-4000-8000-000000000003" || res.Status != "verifying" || res.ReviewURL != "https://x/r/t" {
		t.Fatalf("Submit = %+v, %v", res, err)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/submissions" || rec.encoding != "gzip" ||
		rec.ctype != "application/json" || rec.body != body {
		t.Fatalf("request = %+v", rec)
	}

	// An accepted submission must come back with its id.
	srv, _ = submitServer(t, 202, `{"status":"verifying"}`, nil)
	if _, err := New(srv.Client(), "ua").Submit(t.Context(), Target{BaseURL: srv.URL}, []byte(body)); err == nil {
		t.Fatal("an answer with no id was accepted")
	}
}

func TestAddPart(t *testing.T) {
	const id = "c0000000-0000-4000-8000-000000000003"
	srv, rec := submitServer(t, 202, ``, nil)
	res, err := New(srv.Client(), "ua").AddPart(t.Context(), Target{BaseURL: srv.URL, APIKey: "k"}, id, []byte(`{"device_id":"x"}`))
	if err != nil || res.ID != id {
		t.Fatalf("AddPart = %+v, %v", res, err)
	}
	if rec.path != "/api/v1/submissions/"+id+"/parts" || rec.body != `{"device_id":"x"}` || rec.encoding != "gzip" {
		t.Fatalf("request = %+v", rec)
	}
}

// Each refusal is mapped to the reason the CLI explains; answers that
// aren't the server's JSON (a school filter, a proxy) aren't.
func TestSubmitErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   error
		text   string
	}{
		{"banned", 403, `{"error":"your Hackatime account is banned, so you can't submit hours"}`, nil, ErrBanned, "banned"},
		{"filter 403", 403, `<html>blocked</html>`, nil, nil, "403"},
		{"range", 400, `{"error":"to must be 2026-11-19 or earlier"}`, nil, ErrRefused, "2026-11-19"},
		{"program", 404, `{"error":"program not found"}`, nil, ErrNotFound, "program not found"},
		{"nothing", 409, `{"error":"no counted minutes in this range"}`, nil, ErrConflict, "no counted minutes"},
		{"too large", 413, `{"error":"request body too large"}`, nil, ErrTooLarge, "too large"},
		{"key", 401, `{"error":"unauthorized"}`, nil, ErrKeyRejected, "logbook login"},
		{"rate", 429, `{"error":"slow down"}`, map[string]string{"Retry-After": "120"}, nil, "try again in 2m0s"},
		{"down", 503, ``, nil, nil, "try again later"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := submitServer(t, tt.status, tt.body, tt.header)
			c := New(srv.Client(), "ua")
			_, err := c.Submit(t.Context(), Target{BaseURL: srv.URL}, []byte(`{}`))
			_, perr := c.Plan(t.Context(), Target{BaseURL: srv.URL}, PlanQuery{Project: "p", From: "2026-11-01", To: "2026-11-02"})
			for _, err := range []error{err, perr} {
				var se *SubmitError
				var e *Error
				if !errors.As(err, &se) || !errors.As(err, &e) || e.Status != tt.status {
					t.Fatalf("err = %#v", err)
				}
				if tt.want != nil && !errors.Is(err, tt.want) {
					t.Fatalf("err = %v, want %v", err, tt.want)
				}
				if tt.want == nil && se.Reason != nil {
					t.Fatalf("err = %v, want no reason", err)
				}
				if !strings.Contains(err.Error(), tt.text) {
					t.Fatalf("err = %q, want it to say %q", err, tt.text)
				}
			}
		})
	}
}
