package localapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/status"
)

// statusBar proxies GET .../statusbar/today to the Logbook server, caching
// the answer, and falls back to a local estimate when the server can't be
// reached. Editors poll it, so it must always answer quickly with 200.
type statusBar struct {
	opt Options

	mu       sync.Mutex
	body     []byte
	cachedAt time.Time
	cacheKey string // the API key the cached answer belongs to
	// failNote, when non-empty, is a cached failure ("offline", "re-login
	// needed") from failedAt; it is served until StatusBarTTL passes so a
	// dropped network doesn't make every editor poll wait for a timeout.
	failNote string
	failedAt time.Time
	// fetching is set while one request is out; other callers don't wait
	// for it (single flight) but get the last answer or the estimate.
	fetching bool
}

func newStatusBar(o Options) *statusBar { return &statusBar{opt: o} }

func (s *Server) statusBar(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, note := s.sb.today(r.Context())
	if body == nil {
		body = s.sb.estimate(r.Context(), note)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) // the editor hung up; nothing to do
}

// today returns the server's answer (cached for StatusBarTTL), or nil and a
// short note for the fallback text. At most one upstream request is in
// flight; the mutex is never held across it.
func (sb *statusBar) today(ctx context.Context) ([]byte, string) {
	cfg := sb.opt.Settings()
	sb.mu.Lock()
	now := sb.opt.Now()
	sameKey := sb.cacheKey == cfg.APIKey
	fresh := sameKey && now.Sub(sb.cachedAt) < sb.opt.StatusBarTTL
	switch {
	case fresh && sb.body != nil:
		body := sb.body
		sb.mu.Unlock()
		return body, ""
	case fresh && sb.failNote != "" && now.Sub(sb.failedAt) < sb.opt.StatusBarTTL:
		note := sb.failNote
		sb.mu.Unlock()
		return nil, note
	case sb.fetching:
		// Someone else is asking the server: answer now with what we have.
		var body []byte
		if sameKey {
			body = sb.body
		}
		sb.mu.Unlock()
		return body, ""
	}
	sb.fetching = true
	sb.mu.Unlock()

	body, note := sb.ask(ctx, cfg.ServerURL, cfg.APIKey)

	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.fetching = false
	sb.cachedAt, sb.cacheKey = now, cfg.APIKey
	if body != nil {
		sb.body, sb.failNote, sb.failedAt = body, "", time.Time{}
		return body, ""
	}
	sb.body, sb.failNote, sb.failedAt = nil, note, now
	return nil, note
}

// ask makes one upstream request and returns a valid body, or nil and the
// fallback note.
func (sb *statusBar) ask(ctx context.Context, serverURL, key string) ([]byte, string) {
	body, status, err := sb.fetch(ctx, serverURL, key)
	switch {
	case err != nil:
		sb.opt.Logger.DebugContext(ctx, "status bar: server unreachable", "err", err)
		return nil, "offline"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, "re-login needed"
	case status != http.StatusOK || !json.Valid(body):
		return nil, "offline"
	}
	return body, ""
}

func (sb *statusBar) fetch(ctx context.Context, serverURL, key string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, sb.opt.StatusBarTimeout)
	defer cancel()
	u := strings.TrimRight(serverURL, "/") + "/api/v1/users/current/statusbar/today"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", sb.opt.UserAgent)
	resp, err := sb.opt.Client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetching status bar: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading status bar: %w", err)
	}
	return body, resp.StatusCode, nil
}

// estimate builds a WakaTime-shaped status bar from heartbeats logged
// locally today, with note (e.g. "offline") appended to the total's text.
// It never fails: without data it reports 0 secs.
func (sb *statusBar) estimate(ctx context.Context, note string) []byte {
	now := sb.opt.Now()
	start := status.StartOfDay(now)
	times, err := sb.opt.Store.HeartbeatTimes(ctx, start)
	if err != nil {
		sb.opt.Logger.WarnContext(ctx, "status bar: reading local heartbeats", "err", err)
	}
	sec := int64(status.CodingTime(times).Seconds())
	text := status.DurationText(sec)
	if note != "" {
		text += " (" + note + ")"
	}
	h, m := sec/3600, sec%3600/60
	counter := map[string]any{
		"decimal": fmt.Sprintf("%.2f", float64(h)+float64(m)/60), "digital": fmt.Sprintf("%d:%02d", h, m),
		"hours": h, "minutes": m, "text": text, "total_seconds": sec,
	}
	coding := map[string]any{"name": "Coding", "percent": 100, "seconds": sec % 60}
	for k, v := range counter {
		coding[k] = v
	}
	empty := []any{}
	out := map[string]any{
		"cached_at": now.UTC().Format(time.RFC3339),
		"data": map[string]any{
			"categories": []any{coding}, "dependencies": empty, "editors": empty, "grand_total": counter,
			"languages": empty, "machines": empty, "operating_systems": empty, "projects": empty,
			"range": map[string]any{
				"date": start.Format(time.DateOnly), "start": start.UTC().Format(time.RFC3339),
				"end":      start.AddDate(0, 0, 1).Add(-time.Second).UTC().Format(time.RFC3339),
				"text":     start.Format("Mon Jan 2 2006"),
				"timezone": start.Location().String(),
			},
		},
		"has_team_features": false,
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Marshalling maps of basic values cannot fail; keep a valid answer anyway.
		return []byte(`{"data":{"grand_total":{"text":"0 secs","total_seconds":0}}}`)
	}
	return b
}
