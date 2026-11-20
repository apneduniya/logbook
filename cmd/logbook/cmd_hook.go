package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/harness"
	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/hookrun"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/internal/agent/harness/localpost"
	"github.com/apneduniya/logbook/internal/agent/localkey"
	"github.com/apneduniya/logbook/pkg/wire"
)

// hookCommands returns the commands the harness module owns.
func hookCommands() []Command {
	return []Command{{
		Name:  "hook",
		Usage: "record one AI tool event (run by AI tools): hook <harness> <event>",
		Run:   runHook,
	}}
}

// runHook always returns nil, so the process exits 0 and never blocks or
// breaks the AI tool that ran it. Problems go to the agent log only:
// harnesses read the hook's stdout and stderr.
func runHook(ctx context.Context, args []string) error {
	logFile := hookrun.NewLazyFile(func() (string, error) {
		dir, err := config.Dir()
		return config.LogPath(dir), err
	})
	defer func() { _ = logFile.Close() }() // the log is best effort
	log := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelWarn})).With("component", "hook")

	var (
		name, event string
		parser      hookrun.Parser
		h           wire.Harness
	)
	if len(args) == 2 {
		name, event = args[0], args[1]
		if hk, ok := harness.Lookup(name); ok {
			parser, h = hk, hk.Harness
		}
	}
	cfg := sync.OnceValues(func() (hookConfig, error) { return loadHookConfig(log) })
	r := &hookrun.Runner{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Now:     time.Now,
		Getenv:  os.Getenv,
		Project: func(cwd string) string { return aievent.ProjectName(cwd, hostenv.Exists) },
		Sender: func() (hookrun.Sender, error) {
			c, err := cfg()
			if err != nil {
				return nil, err
			}
			return localpost.New(c.LocalAddr, c.LocalToken, config.SpoolPath(c.dir), log), nil
		},
		PromptKey: func() ([]byte, error) {
			c, err := cfg()
			if err != nil {
				return nil, err
			}
			return localkey.PromptKey(c.LocalToken)
		},
		Log: func(msg string, kv ...any) { log.Warn(msg, kv...) },
	}
	r.Run(ctx, h, parser, event)
	return nil
}

// hookConfig is the agent config as a hook needs it, plus the data dir.
type hookConfig struct {
	config.Config
	dir string
}

// loadHookConfig reads the agent config once per hook. Without a usable
// config (not installed yet, or unreadable) the config is empty: events go
// straight to the spool, which the daemon drains once it runs, and prompts
// get no hash (there is no local token to key it with).
func loadHookConfig(log *slog.Logger) (hookConfig, error) {
	dir, err := config.Dir()
	if err != nil {
		return hookConfig{}, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		log.Debug("no usable config, spooling", "err", err)
		cfg = config.Config{}
	}
	return hookConfig{Config: cfg, dir: dir}, nil
}
