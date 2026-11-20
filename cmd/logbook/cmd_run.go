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
	"github.com/apneduniya/logbook/internal/agent/logfile"
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

	err = daemon.Run(ctx, daemon.Options{
		DataDir: dir, Config: cfg, Logger: log,
		Version: version.String(), UserAgent: version.UserAgent(),
	})
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		// Autostart and a manual start can race; the second one just exits.
		fmt.Fprintln(os.Stderr, "logbook: the agent is already running")
		return nil
	}
	return err
}

// bestEffortTee writes to primary and, ignoring failures, to secondary. A
// daemon started at login on Windows may have no usable stderr, and that
// must not stop logging to the file.
type bestEffortTee struct{ primary, secondary io.Writer }

func (t bestEffortTee) Write(p []byte) (int, error) {
	_, _ = t.secondary.Write(p) // see type comment
	return t.primary.Write(p)
}
