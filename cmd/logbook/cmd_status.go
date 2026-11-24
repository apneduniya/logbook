package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/version"
)

func statusCommand() Command {
	return Command{
		Name:  "status",
		Usage: "show whether the agent is running and what it has sent [--json]",
		Run: func(ctx context.Context, args []string) error {
			return showStatus(ctx, args, os.Stdout)
		},
	}
}

func showStatus(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	snap, err := currentStatus(ctx, dir, cfg)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snap); err != nil {
			return fmt.Errorf("writing status: %w", err)
		}
		return nil
	}
	var b strings.Builder
	printStatus(&b, snap, cfg.LocalAddr)
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		return fmt.Errorf("writing status: %w", err)
	}
	return nil
}

// currentStatus asks the running daemon, falling back to reading the local
// database directly when it isn't running.
func currentStatus(ctx context.Context, dir string, cfg config.Config) (status.Snapshot, error) {
	if s, err := status.Fetch(ctx, &http.Client{Timeout: 3 * time.Second}, cfg.LocalAddr); err == nil {
		return s, nil
	}
	return offlineStatus(ctx, dir, cfg)
}

func offlineStatus(ctx context.Context, dir string, cfg config.Config) (status.Snapshot, error) {
	in := status.Input{Config: cfg, DataDir: dir, Version: version.String(), Now: time.Now()}
	if _, err := os.Stat(config.DBPath(dir)); errors.Is(err, fs.ErrNotExist) {
		// Never started: report an empty snapshot rather than creating a database.
		in.Store = emptyStore{}
		return status.Collect(ctx, in)
	}
	db, err := localdb.Open(ctx, config.DBPath(dir))
	if err != nil {
		return status.Snapshot{}, err
	}
	defer func() { _ = db.Close() }()
	in.Store = db
	return status.Collect(ctx, in)
}

// emptyStore stands in for a database that doesn't exist yet.
type emptyStore struct{}

func (emptyStore) Stats(context.Context, localdb.Queue) (localdb.QueueStats, error) {
	return localdb.QueueStats{}, nil
}

func (emptyStore) CountSince(context.Context, time.Time) (localdb.Counts, error) {
	return localdb.Counts{}, nil
}

func (emptyStore) HeartbeatTimes(context.Context, time.Time) ([]time.Time, error) { return nil, nil }

func (emptyStore) Prefix(context.Context, string) (map[string]string, error) { return nil, nil }

// printStatus formats s into w, an in-memory builder (so writes can't fail).
func printStatus(w *strings.Builder, s status.Snapshot, addr string) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	if s.DaemonRunning {
		p("Agent:      running at http://%s (status page)", addr)
	} else {
		p("Agent:      not running; start it with `logbook run`")
	}
	signedIn := "no; run `logbook login --key <key>`"
	if s.SignedIn {
		signedIn = "yes"
	}
	p("Server:     %s (signed in: %s)", s.ServerURL, signedIn)
	printUploader(p, "Logbook", s.Logbook)
	if s.MirrorHost != "" {
		printUploader(p, "Hackatime", s.Hackatime)
	} else {
		p("Hackatime:  mirroring off")
	}
	printCrediting(p, s.Crediting)
	p("Today:      %s coding (local estimate), %d heartbeats, %d AI events, %d git events",
		s.Today.CodingText, s.Today.Heartbeats, s.Today.AIEvents, s.Today.GitEvents)
	if s.SpoolBytes > 0 {
		p("Waiting:    %d bytes of hook events in the spool", s.SpoolBytes)
	}
	if s.Hooks == nil {
		p("AI hooks:   unknown")
	}
	for _, h := range s.Hooks {
		p("AI hooks:   %s %s", h.Name, h.State)
	}
}

func printCrediting(p func(string, ...any), c crediting.Status) {
	switch {
	case !c.Device.RevokedAt.IsZero():
		p("Device:     %s (revoked)", c.Device.ID)
	case c.Device.ID != "":
		p("Device:     %s", c.Device.ID)
	default:
		p("Device:     not registered yet")
	}
	last := "never"
	if !c.Minutes.LastSuccess.IsZero() {
		last = c.Minutes.LastSuccess.Local().Format(time.DateTime)
	}
	p("Credited:   %d minutes waiting, last upload %s", c.Minutes.Waiting, last)
	if c.Minutes.Paused != "" {
		p("            paused: %s", c.Minutes.Paused)
	}
	if c.Minutes.LastError != "" {
		p("            last error: %s", c.Minutes.LastError)
	}
	if c.Minutes.Refused != "" {
		p("            on hold: %s", c.Minutes.Refused)
	}
	if c.Minutes.Rejected > 0 {
		p("            %d minutes rejected by the server", c.Minutes.Rejected)
	}
	if c.Minutes.SealedError != "" {
		p("            error: %d minutes lost: %s", c.Minutes.SealedRejected, c.Minutes.SealedError)
	}
	if c.Minutes.Clock != "" {
		p("            clock: %s", c.Minutes.Clock)
	}
	if c.Minutes.BelowFloor != "" {
		p("            paused: %s", c.Minutes.BelowFloor)
	}
	if c.Minutes.Settling != "" {
		p("            settling: %s", c.Minutes.Settling)
	}
	if c.Minutes.LateIgnored > 0 {
		p("            %d events arrived after their minute was final: kept as evidence, not credited (latest for %s)",
			c.Minutes.LateIgnored, c.Minutes.LateLatest.Local().Format("2006-01-02 15:04"))
	}
	if c.Minutes.Unkeyed > 0 {
		p("            %d minutes kept as the server holds them, from before minutes recorded how they were computed",
			c.Minutes.Unkeyed)
	}
	if c.Minutes.DroppedSamples > 0 {
		p("            %d activity samples dropped: the local log is full of rows not sealed as evidence yet",
			c.Minutes.DroppedSamples)
	}
	switch {
	case c.Evidence.State != "":
		p("Evidence:   stopped: %s", c.Evidence.Error)
	case c.Evidence.LastDay != "":
		p("Evidence:   signed through %s", c.Evidence.LastDay)
	default:
		p("Evidence:   no day sealed yet")
	}
	if c.Evidence.Sealing != "" {
		p("            sealing %s: closed to new minutes, its evidence is being hashed", c.Evidence.Sealing)
	}
	if c.Evidence.LocalError != "" {
		p("            sealing failed (retrying): %s", c.Evidence.LocalError)
	}
}

func printUploader(p func(string, ...any), name string, u status.Uploader) {
	last := "never"
	if !u.Worker.LastSuccess.IsZero() {
		last = u.Worker.LastSuccess.Local().Format(time.DateTime)
	}
	p("%-11s %d waiting, last upload %s", name+":", u.Queue.Pending, last)
	if u.Worker.Paused != "" {
		p("            paused: %s", u.Worker.Paused)
	}
	if u.Worker.LastError != "" {
		p("            last error: %s", u.Worker.LastError)
	}
	if u.Queue.Dropped > 0 {
		p("            %d dropped because the queue was full", u.Queue.Dropped)
	}
}
