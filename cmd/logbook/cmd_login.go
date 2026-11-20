package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/apneduniya/logbook/internal/agent/account"
	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/daemon"
	"github.com/apneduniya/logbook/internal/agent/doctor"
	"github.com/apneduniya/logbook/internal/agent/version"
)

const maxKeyLen = 256

func loginCommand() Command {
	return Command{
		Name:  "login",
		Usage: "save your Logbook API key: login --key <key|-> [--server <url>]",
		Run: func(ctx context.Context, args []string) error {
			return login(ctx, args, os.Stdin, os.Stdout)
		},
	}
}

func login(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	key := fs.String("key", "", "your Logbook API key, or - to read it from stdin (keeps it out of shell history)")
	server := fs.String("server", "", "Logbook server URL (default: keep the current one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k := strings.TrimSpace(*key)
	if k == "-" {
		line, err := bufio.NewReader(io.LimitReader(stdin, maxKeyLen+2)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading key from stdin: %w", err)
		}
		k = strings.TrimSpace(line)
	}
	if k == "" || len(k) > maxKeyLen || strings.ContainsAny(k, " \t\r\n") {
		return errors.New("pass your API key with --key <key> (find it on your Logbook dashboard)")
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if errors.Is(err, config.ErrNotConfigured) {
		cfg, err = config.Default()
	}
	if err != nil {
		return err
	}
	if *server != "" {
		u, err := url.Parse(*server)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("--server %q is not an http(s) URL", *server)
		}
		cfg.ServerURL = strings.TrimRight(*server, "/")
	}

	if _, err := account.Verify(ctx, daemon.NewHTTPClient(), cfg.ServerURL, k, version.UserAgent()); err != nil {
		if errors.Is(err, account.ErrKeyRejected) {
			return fmt.Errorf("%s rejected this key; copy it again from your Logbook dashboard", cfg.ServerURL)
		}
		return fmt.Errorf("could not check the key: %w (nothing was saved)", err)
	}
	cfg.APIKey = k
	if err := config.Save(dir, cfg); err != nil {
		return err
	}
	msg := fmt.Sprintf("Signed in to %s.\nA running agent picks up the new key within a few seconds.\n", cfg.ServerURL)
	msg += editorKeyHint(cfg)
	if _, err := io.WriteString(stdout, msg); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

// editorKeyHint tells the user to re-run `logbook install` when editors
// point at the agent but still send a different key: the local API accepts
// editor heartbeats only with the agent's Logbook key, so after a key change
// they would get 401 until .wakatime.cfg is updated. The installer owns that
// file, so login only reads it.
func editorKeyHint(cfg config.Config) string {
	path, err := doctor.WakatimeCfgPath()
	if err != nil {
		return ""
	}
	w := doctor.ReadWakatimeCfg(path, cfg.APIKey)
	if w.Err != nil || w.KeyMatches || !doctor.PointsAtAgent(w.APIURL, cfg.LocalAddr) {
		return ""
	}
	return "Your editors still send a different key (" + path +
		"); run `logbook install` so their heartbeats reach the agent.\n"
}
