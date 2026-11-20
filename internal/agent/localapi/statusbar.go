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
	cacheKey string // the API key the cached body belongs to
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
// short note for the fallback text.
func (sb *statusBar) today(ctx context.Context) ([]byte, string) {
	cfg := sb.opt.Settings()
	sb.mu.Lock()
	defer sb.mu.Unlock()
	now := sb.opt.Now()
	if sb.body != nil && sb.cacheKey == cfg.APIKey && now.Sub(sb.cachedAt) < sb.opt.StatusBarTTL {
		return sb.body, ""
	}
	body, status, err := sb.fetch(ctx, cfg.ServerURL, cfg.APIKey)
	switch {
	case err != nil:
		sb.opt.Logger.DebugContext(ctx, "status bar: server unreachable", "err", err)
		return nil, "offline"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, "re-login needed"
	case status != http.StatusOK || !json.Valid(body):
		return nil, "offline"
	}
	sb.body, sb.cachedAt, sb.cacheKey = body, now, cfg.APIKey
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
