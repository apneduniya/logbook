package hackatime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClientValidatesBaseURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		{"", false}, // default
		{"https://hackatime.hackclub.com", false},
		{"https://hackatime.example/", false},
		{"http://127.0.0.1:3000", false},
		{"http://localhost:3000", false},
		{"http://hackatime.hackclub.com", true},
		{"hackatime.hackclub.com", true},
		{"ftp://x", true},
		{"https://", true},
	}
	for _, tt := range tests {
		_, err := NewClient(ClientConfig{BaseURL: tt.url})
		if (err != nil) != tt.wantErr {
			t.Errorf("NewClient(%q) err = %v, wantErr %v", tt.url, err, tt.wantErr)
		}
	}
}

func TestMeAndTrust(t *testing.T) {
	f := newFakeHackatime(t)
	f.addUser(42, "green")
	c := f.client(t, nil)
	tok := f.tokenFor(42)

	me, err := c.Me(context.Background(), tok)
	if err != nil {
		t.Fatalf("Me() err = %v", err)
	}
	if me.ID != 42 || me.Trust != TrustGreen || me.GitHubUsername != "gh42" {
		t.Fatalf("Me() = %+v", me)
	}
	if _, err := c.Me(context.Background(), "nope"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Me(bad token) err = %v, want ErrUnauthorized", err)
	}

	// The token owner sees their own raw level, including "suspected".
	f.setTrust(42, "yellow")
	if me, err := c.Me(context.Background(), tok); err != nil || me.Trust != TrustYellow {
		t.Fatalf("Me() trust = %q, %v; want yellow", me.Trust, err)
	}
	for _, ua := range f.userAgents {
		if ua != UserAgent {
			t.Fatalf("User-Agent = %q, want %q", ua, UserAgent)
		}
	}
}

// serve returns a client against a server that answers each request with fn.
func serve(t *testing.T, fn func(w http.ResponseWriter, r *http.Request, call int)) *Client {
	t.Helper()
	f := newFakeHackatime(t)
	var calls atomic.Int32
	f.meHook = func(w http.ResponseWriter, r *http.Request) bool {
		fn(w, r, int(calls.Add(1)))
		return true
	}
	f.hbHook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		fn(w, r, int(calls.Add(1)))
		return true
	}
	return f.client(t, nil)
}

func TestMalformedResponsesRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `<html>oops</html>`},
		{"truncated", `{"id": 1, "trust_factor": {"trust_level": "green"`},
		{"trailing data", `{"id":1,"trust_factor":{"trust_level":"green","trust_value":2}} {"id":2}`},
		{"id wrong type", `{"id":"1","trust_factor":{"trust_level":"green","trust_value":2}}`},
		{"missing id", `{"trust_factor":{"trust_level":"green","trust_value":2}}`},
		{"zero id", `{"id":0,"trust_factor":{"trust_level":"green","trust_value":2}}`},
		{"unknown trust level", `{"id":1,"trust_factor":{"trust_level":"purple","trust_value":9}}`},
		{"trust value mismatch", `{"id":1,"trust_factor":{"trust_level":"green","trust_value":1}}`},
		{"missing trust value", `{"id":1,"trust_factor":{"trust_level":"red"}}`},
		{"array", `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tt.body))
			})
			if _, err := c.Me(context.Background(), "t"); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Me() err = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestMalformedHeartbeatsRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing heartbeats", `{"start_time":"x","total_seconds":0}`},
		{"heartbeats not array", `{"heartbeats":{}}`},
		{"time as string", `{"heartbeats":[{"time":"1700000000","entity":"a"}]}`},
		{"missing time", `{"heartbeats":[{"entity":"a"}]}`},
		{"lines as string", `{"heartbeats":[{"time":1700000000,"entity":"a","lines":"ten"}]}`},
		{"dependencies wrong type", `{"heartbeats":[{"time":1700000000,"entity":"a","dependencies":"fmt"}]}`},
	}
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				_, _ = w.Write([]byte(tt.body))
			})
			if _, err := c.Heartbeats(context.Background(), "t", from, from.Add(time.Hour)); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Heartbeats() err = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestRetries(t *testing.T) {
	tests := []struct {
		name      string
		responses []func(w http.ResponseWriter)
		wantErr   func(error) bool
		wantCalls int
	}{
		{
			name: "429 with Retry-After then ok",
			responses: []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { w.Header().Set("Retry-After", "1"); w.WriteHeader(429) },
				okMe,
			},
			wantErr:   func(err error) bool { return err == nil },
			wantCalls: 2,
		},
		{
			name: "503 twice then ok",
			responses: []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { w.WriteHeader(503) },
				func(w http.ResponseWriter) { w.WriteHeader(502) },
				okMe,
			},
			wantErr:   func(err error) bool { return err == nil },
			wantCalls: 3,
		},
		{
			name: "Retry-After beyond limit is returned to caller",
			responses: []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { w.Header().Set("Retry-After", "120"); w.WriteHeader(429) },
			},
			wantErr: func(err error) bool {
				var rl *RateLimitedError
				return errors.As(err, &rl) && rl.RetryAfter == 120*time.Second
			},
			wantCalls: 1,
		},
		{
			name: "400 is not retried",
			responses: []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { w.WriteHeader(400) },
			},
			wantErr: func(err error) bool {
				var se *StatusError
				return errors.As(err, &se) && se.Code == 400
			},
			wantCalls: 1,
		},
		{
			name: "403 is unauthorized",
			responses: []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { w.WriteHeader(403) },
			},
			wantErr:   func(err error) bool { return errors.Is(err, ErrUnauthorized) },
			wantCalls: 1,
		},
		{
			name: "redirects are not followed",
			responses: []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { w.Header().Set("Location", "https://evil.example/"); w.WriteHeader(302) },
			},
			wantErr: func(err error) bool {
				var se *StatusError
				return errors.As(err, &se) && se.Code == 302
			},
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got atomic.Int32
			c := serve(t, func(w http.ResponseWriter, _ *http.Request, call int) {
				got.Store(int32(call))
				tt.responses[min(call, len(tt.responses))-1](w)
			})
			_, err := c.Me(context.Background(), "t")
			if !tt.wantErr(err) {
				t.Fatalf("Me() err = %v", err)
			}
			if int(got.Load()) != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", got.Load(), tt.wantCalls)
			}
		})
	}
}

func okMe(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"id":5,"trust_factor":{"trust_level":"blue","trust_value":0}}`))
}

func TestRetriesExhaustedOn5xx(t *testing.T) {
	var calls atomic.Int32
	f := newFakeHackatime(t)
	f.meHook = func(w http.ResponseWriter, _ *http.Request) bool {
		calls.Add(1)
		w.WriteHeader(500)
		return true
	}
	c := f.client(t, func(cfg *ClientConfig) { cfg.MaxRetries = 1 })
	var se *StatusError
	if _, err := c.Me(context.Background(), "t"); !errors.As(err, &se) || se.Code != 500 {
		t.Fatalf("Me() err = %v, want 500", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestResponseTooLarge(t *testing.T) {
	f := newFakeHackatime(t)
	f.hbHook = func(w http.ResponseWriter, _ *http.Request, _ int) bool {
		_, _ = fmt.Fprintf(w, `{"heartbeats":[{"time":1700000000,"entity":%q}]}`, strings.Repeat("x", 4096))
		return true
	}
	c := f.client(t, func(cfg *ClientConfig) { cfg.MaxHeartbeatsBody = 1024 })
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := c.Heartbeats(context.Background(), "t", from, from.Add(time.Hour)); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Heartbeats() err = %v, want ErrResponseTooLarge", err)
	}
}

func TestBearerAndQuery(t *testing.T) {
	f := newFakeHackatime(t)
	var auth, start, end string
	f.hbHook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		auth, start, end = r.Header.Get("Authorization"), r.URL.Query().Get("start_time"), r.URL.Query().Get("end_time")
		_, _ = w.Write([]byte(`{"heartbeats":[]}`))
		return true
	}
	c := f.client(t, nil)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := c.Heartbeats(context.Background(), "secret-tok", from, from.Add(24*time.Hour-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret-tok" || start != "2025-01-01T00:00:00Z" || end != "2025-01-01T23:59:59.999999Z" {
		t.Fatalf("auth=%q start=%q end=%q", auth, start, end)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{" 7 ", 7 * time.Second},
		{"-3", 0},
		{"soon", 0},
		{"99999999", 24 * time.Hour},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestExchange(t *testing.T) {
	f := newFakeHackatime(t)
	f.addUser(42, "green")
	c := f.client(t, nil)
	ctx := context.Background()

	authURL, err := c.AuthCodeURL("state-1", "verifier-verifier-verifier-verifier-verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	code, state := f.authorize(authURL, 42)
	if state != "state-1" {
		t.Fatalf("state = %q", state)
	}
	if _, err := c.Exchange(ctx, code, "a-different-verifier-a-different-verifier-xx"); !errors.Is(err, ErrAuthorizationRejected) {
		t.Fatalf("Exchange(wrong verifier) err = %v, want ErrAuthorizationRejected", err)
	}

	code, _ = f.authorize(authURL, 42)
	tok, err := c.Exchange(ctx, code, "verifier-verifier-verifier-verifier-verifier-1")
	if err != nil || tok == "" {
		t.Fatalf("Exchange() = %q, %v", tok, err)
	}
	if _, err := c.Exchange(ctx, code, "verifier-verifier-verifier-verifier-verifier-1"); !errors.Is(err, ErrAuthorizationRejected) {
		t.Fatalf("Exchange(reused code) err = %v, want ErrAuthorizationRejected", err)
	}

	unconfigured, err := NewClient(ClientConfig{BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unconfigured.AuthCodeURL("s", "v"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("AuthCodeURL(unconfigured) err = %v", err)
	}
}
