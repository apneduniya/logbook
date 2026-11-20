package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A captive portal redirects every request to its login page. An upload
// POST must see the 3xx (and keep its batch) instead of a GET's 200 page.
func TestHTTPClientDoesNotFollowRedirectsForPOST(t *testing.T) {
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = w.Write([]byte("<html>sign in</html>"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer portal.Close()
	client := NewHTTPClient()

	tests := []struct {
		method, path string
		body         io.Reader
		want         int
	}{
		{http.MethodPost, "/api/v1/users/current/heartbeats.bulk", strings.NewReader("[]"), http.StatusFound},
		{http.MethodGet, "/api/v1/users/current/statusbar/today", nil, http.StatusOK},
	}
	for _, tt := range tests {
		req, err := http.NewRequestWithContext(t.Context(), tt.method, portal.URL+tt.path, tt.body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Fatalf("%s status = %d, want %d", tt.method, resp.StatusCode, tt.want)
		}
	}
}
