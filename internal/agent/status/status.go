package status

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/upload"
)

// Snapshot is the agent's health at one moment.
type Snapshot struct {
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	// DaemonRunning is false when the CLI built this from the database alone.
	DaemonRunning bool      `json:"daemon_running"`
	StartedAt     time.Time `json:"started_at,omitzero"`

	ServerURL string `json:"server_url"`
	SignedIn  bool   `json:"signed_in"`
	// MirrorHost is the Hackatime host heartbeats are mirrored to ("" if off).
	MirrorHost string `json:"mirror_host,omitempty"`

	Logbook   Uploader `json:"logbook"`
	Hackatime Uploader `json:"hackatime"`

	// Hooks lists AI harness hooks; nil means the installer did not report.
	Hooks []Hook `json:"hooks"`

	Today      Today `json:"today"`
	SpoolBytes int64 `json:"spool_bytes"`
}

// Uploader is one upload queue and the worker draining it.
type Uploader struct {
	Queue  localdb.QueueStats `json:"queue"`
	Worker upload.State       `json:"worker"`
}

// Hook is one AI harness's hook installation state.
type Hook struct {
	Name string `json:"name"`
	// State is "installed", "not installed" or "error: ...".
	State string `json:"state"`
}

// Today is what was recorded locally since local midnight.
type Today struct {
	localdb.Counts
	// CodingSeconds is a local estimate from heartbeat gaps (see CodingTime).
	CodingSeconds int64  `json:"coding_seconds"`
	CodingText    string `json:"coding_text"`
}

// Harness is what the status needs from an installed AI harness. The
// installer's harness values satisfy it.
type Harness interface {
	Name() string
	Installed() (bool, error)
}

// Store is the database access Collect needs. *localdb.DB satisfies it.
type Store interface {
	Stats(ctx context.Context, q localdb.Queue) (localdb.QueueStats, error)
	CountSince(ctx context.Context, since time.Time) (localdb.Counts, error)
	HeartbeatTimes(ctx context.Context, since time.Time) ([]time.Time, error)
	Prefix(ctx context.Context, prefix string) (map[string]string, error)
}

// Input is everything Collect reads.
type Input struct {
	Store   Store
	Config  config.Config
	DataDir string
	Version string
	// Harnesses may be nil when unknown.
	Harnesses []Harness
	Now       time.Time
}

// Collect builds a Snapshot from the database and config.
func Collect(ctx context.Context, in Input) (Snapshot, error) {
	s := Snapshot{
		Version: in.Version, GeneratedAt: in.Now.UTC(),
		ServerURL: in.Config.ServerURL, SignedIn: in.Config.APIKey != "",
	}
	if upload.MirrorEnabled(in.Config) {
		if u, err := url.Parse(in.Config.Hackatime.URL); err == nil {
			s.MirrorHost = u.Host
		}
	}
	for _, u := range []struct {
		dst  *Uploader
		q    localdb.Queue
		name string
	}{{&s.Logbook, localdb.HeartbeatQueue, NameLogbook}, {&s.Hackatime, localdb.MirrorQueue, NameHackatime}} {
		st, err := in.Store.Stats(ctx, u.q)
		if err != nil {
			return Snapshot{}, err
		}
		ws, err := upload.LoadState(ctx, in.Store, u.name)
		if err != nil {
			return Snapshot{}, err
		}
		*u.dst = Uploader{Queue: st, Worker: ws}
	}
	if in.Harnesses != nil {
		s.Hooks = make([]Hook, 0, len(in.Harnesses))
		for _, h := range in.Harnesses {
			s.Hooks = append(s.Hooks, hookState(h))
		}
	}

	midnight := StartOfDay(in.Now)
	counts, err := in.Store.CountSince(ctx, midnight)
	if err != nil {
		return Snapshot{}, err
	}
	times, err := in.Store.HeartbeatTimes(ctx, midnight)
	if err != nil {
		return Snapshot{}, err
	}
	secs := int64(CodingTime(times).Seconds())
	s.Today = Today{Counts: counts, CodingSeconds: secs, CodingText: DurationText(secs)}

	if in.DataDir != "" {
		if fi, err := os.Stat(config.SpoolPath(in.DataDir)); err == nil {
			s.SpoolBytes = fi.Size()
		}
	}
	return s, nil
}

// Worker names, shared by the daemon and status readers.
const (
	NameLogbook   = "logbook"
	NameHackatime = "hackatime"
)

func hookState(h Harness) Hook {
	ok, err := h.Installed()
	switch {
	case err != nil:
		return Hook{Name: h.Name(), State: fmt.Sprintf("error: %v", err)}
	case ok:
		return Hook{Name: h.Name(), State: "installed"}
	}
	return Hook{Name: h.Name(), State: "not installed"}
}

// StartOfDay returns local midnight of t's day.
func StartOfDay(t time.Time) time.Time {
	l := t.Local()
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.Local)
}
