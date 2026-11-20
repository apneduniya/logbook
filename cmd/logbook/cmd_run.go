package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/daemon"
	"github.com/apneduniya/logbook/internal/agent/harness"
	"github.com/apneduniya/logbook/internal/agent/install/autostart"
	"github.com/apneduniya/logbook/internal/agent/logfile"
	"github.com/apneduniya/logbook/internal/agent/sensors/activity"
	"github.com/apneduniya/logbook/internal/agent/sensors/gitwatch"
	"github.com/apneduniya/logbook/internal/agent/sensors/osprobe"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/version"
)

func runCommand() Command {
	return Command{
		Name:  "run",
		Usage: "run the agent in the foreground (`logbook install` starts it at login)",
		Run:   runAgent,
	}
}

func runAgent(ctx context.Context, args []string) error {
	// On Windows, a daemon started at login relaunches itself without a
	// console window; closing a visible console would otherwise kill it.
	if bin, err := os.Executable(); err == nil {
		relaunched, err := autostart.Background(bin, append([]string{"run"}, args...))
		if err != nil {
			return fmt.Errorf("starting in the background: %w", err)
		}
		if relaunched {
			return nil
		}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "log only to the log file, not to the terminal")
	debug := fs.Bool("debug", false, "log every upload")
	if err := fs.Parse(args); err != nil {
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
	lf, err := logfile.Open(config.LogPath(dir), logfile.DefaultMaxBytes, logfile.DefaultKeep)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	var out io.Writer = lf
	if !*quiet {
		out = bestEffortTee{primary: lf, secondary: os.Stderr}
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))

	probe := osprobe.New(log.With("sensor", "activity"))
	defer func() { _ = probe.Close() }()
	hs := harness.All()
	statusHarnesses := make([]status.Harness, 0, len(hs))
	for _, h := range hs {
		statusHarnesses = append(statusHarnesses, h)
	}

	err = daemon.Run(ctx, daemon.Options{
		DataDir: dir, Config: cfg, Logger: log,
		Version: version.String(), UserAgent: version.UserAgent(),
		Sensors: []daemon.Sensor{
			activitySensor{activity.New(probe, activity.WithLogger(log.With("sensor", "activity")))},
		},
		RepoSensors: []func(daemon.RepoSource) daemon.Sensor{
			func(r daemon.RepoSource) daemon.Sensor {
				return gitSensor{gitwatch.New(repoSource{r}, gitwatch.WithLogger(log.With("sensor", "git")))}
			},
		},
		Harnesses: statusHarnesses,
	})
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		// Autostart and a manual start can race; the second one just exits.
		fmt.Fprintln(os.Stderr, "logbook: the agent is already running")
		return nil
	}
	return err
}

// Sensors declare their own Sink interfaces so they don't import the daemon;
// these adapters pass the daemon's sink through, which satisfies both.
type activitySensor struct{ s *activity.Sensor }

func (a activitySensor) Name() string { return a.s.Name() }
func (a activitySensor) Run(ctx context.Context, sink daemon.Sink) error {
	return a.s.Run(ctx, sink) //nolint:wrapcheck // the daemon logs sensor errors with the sensor name
}

type gitSensor struct{ w *gitwatch.Watcher }

func (g gitSensor) Name() string { return g.w.Name() }
func (g gitSensor) Run(ctx context.Context, sink daemon.Sink) error {
	return g.w.Run(ctx, sink) //nolint:wrapcheck // the daemon logs sensor errors with the sensor name
}

// repoSource adapts the daemon's active-repo list to the git sensor's own
// interface (the sensor deliberately doesn't import the daemon).
type repoSource struct{ d daemon.RepoSource }

func (r repoSource) ActiveRepos(ctx context.Context) ([]gitwatch.Repo, error) {
	rs, err := r.d.ActiveRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing active repos: %w", err)
	}
	out := make([]gitwatch.Repo, len(rs))
	for i, x := range rs {
		out[i] = gitwatch.Repo{Path: x.Path, Project: x.Project}
	}
	return out, nil
}

// bestEffortTee writes to primary and, ignoring failures, to secondary. A
// daemon started at login on Windows may have no usable stderr, and that
// must not stop logging to the file.
type bestEffortTee struct{ primary, secondary io.Writer }

func (t bestEffortTee) Write(p []byte) (int, error) {
	_, _ = t.secondary.Write(p) // see type comment
	return t.primary.Write(p)
}
