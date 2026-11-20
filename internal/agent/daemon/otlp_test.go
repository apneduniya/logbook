package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localkey"
)

// Claude Code's exporter, configured the way the installer configures it,
// reaches the running daemon and its active time is stored.
func TestDaemonReceivesClaudeCodeActiveTime(t *testing.T) {
	e := newEnv(t)
	stop := e.start(t)
	tok, err := localkey.OTLPToken(e.cfg.LocalToken)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Truncate(time.Minute)
	body := fmt.Sprintf(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"user.email","value":{"stringValue":"teen@example.com"}}]},`+
		`"scopeMetrics":[{"metrics":[{"name":"claude_code.active_time.total","sum":{"aggregationTemporality":1,"dataPoints":[`+
		`{"attributes":[{"key":"session.id","value":{"stringValue":"s1"}},{"key":"type","value":{"stringValue":"user"}}],`+
		`"startTimeUnixNano":"%d","timeUnixNano":"%d","asDouble":42}]}}]}]}]}`,
		end.Add(-time.Minute).UnixNano(), end.UnixNano())
	post := func(token string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+e.cfg.LocalAddr+"/v1/metrics", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Logbook-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(e.cfg.LocalToken); code != http.StatusUnauthorized {
		t.Fatalf("local token accepted for OTLP: %d", code)
	}
	if code := post(tok); code != http.StatusOK {
		t.Fatalf("export status %d", code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, e.dir)
	defer func() { _ = db.Close() }()
	got, err := db.ActiveTimes(context.Background(), end.Add(-time.Hour), end.Add(time.Minute))
	if err != nil || len(got) != 1 || got[0].SessionID != "s1" || got[0].UserSecs != 42 {
		t.Fatalf("ActiveTimes = %+v, %v", got, err)
	}
}
