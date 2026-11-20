package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// fetchTimeout bounds a request to the local daemon.
const fetchTimeout = 2 * time.Second

// Fetch reads the running daemon's snapshot from GET /local/v1/status at
// addr (host:port).
func Fetch(ctx context.Context, client *http.Client, addr string) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/local/v1/status", nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("building request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("reaching the agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("agent answered %d", resp.StatusCode)
	}
	var s Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("reading agent status: %w", err)
	}
	return s, nil
}
