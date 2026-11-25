package main

import (
	"context"
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
	"github.com/apneduniya/logbook/internal/agent/daemon"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/submit"
	"github.com/apneduniya/logbook/internal/agent/version"
	"github.com/apneduniya/logbook/pkg/replay"
)

func submitCommand() Command {
	return Command{
		Name: "submit",
		Usage: "submit a project's hours for review: submit <project> --from YYYY-MM-DD --to YYYY-MM-DD " +
			"[--program <slug>] [--yes] [--preview FILE] [--add <submission-id>]",
		Run: func(ctx context.Context, args []string) error {
			return runSubmit(ctx, args, os.Stdin, os.Stdout, isTerminal(os.Stdin))
		},
	}
}

func runSubmit(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, interactive bool) error {
	req, err := parseSubmit(args)
	if err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if errors.Is(err, config.ErrNotConfigured) || err == nil && cfg.APIKey == "" {
		return errors.New("not signed in; run `logbook login --key <key>` first")
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(config.DBPath(dir)); errors.Is(err, fs.ErrNotExist) {
		return errors.New("this device has no local log yet; start the agent with `logbook run`")
	}
	db, err := localdb.Open(ctx, config.DBPath(dir))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // submit only reads the log; nothing is lost if closing fails
	// The daemon registers the device under the URL without a trailing
	// slash (internal/agent/daemon).
	server := strings.TrimRight(cfg.ServerURL, "/")
	deviceID, err := thisDevice(ctx, db, server)
	if err != nil {
		return err
	}
	return submit.Run(ctx, req, submit.Env{
		Server:   serverapi.New(submitHTTPClient(), version.UserAgent()),
		Target:   serverapi.Target{BaseURL: server, APIKey: cfg.APIKey},
		DeviceID: deviceID,
		Prove: func(ctx context.Context, keys []replay.Key) (crediting.Proofs, error) {
			return crediting.BuildProofs(ctx, db, deviceID, keys)
		},
		In: stdin, Out: stdout, Interactive: interactive,
	})
}

// parseSubmit reads `submit <project> [flags]`; the project may come
// before or after the flags.
func parseSubmit(args []string) (submit.Request, error) {
	var req submit.Request
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.StringVar(&req.From, "from", "", "first UTC day of the range (YYYY-MM-DD)")
	fs.StringVar(&req.To, "to", "", "last UTC day of the range (YYYY-MM-DD); it must be sealed, so yesterday at the latest")
	fs.StringVar(&req.Program, "program", "", "the program's slug, to send the submission to its reviewers")
	fs.BoolVar(&req.Yes, "yes", false, "upload without asking for confirmation")
	fs.StringVar(&req.Preview, "preview", "", "write the request body to FILE instead of uploading it")
	fs.StringVar(&req.AddTo, "add", "", "add this device's evidence to an existing submission (same project and range)")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return submit.Request{}, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 1 {
		return submit.Request{}, errors.New("usage: logbook submit <project> --from YYYY-MM-DD --to YYYY-MM-DD [--program <slug>] [--yes] [--preview FILE] [--add <submission-id>]")
	}
	if req.From == "" || req.To == "" {
		return submit.Request{}, errors.New("give the range with --from YYYY-MM-DD --to YYYY-MM-DD (UTC days)")
	}
	req.Project = pos[0]
	return req, nil
}

// thisDevice returns the device id this agent registered with server.
func thisDevice(ctx context.Context, db *localdb.DB, server string) (string, error) {
	st, err := db.Prefix(ctx, "device.")
	if err != nil {
		return "", fmt.Errorf("reading this device's registration: %w", err)
	}
	switch {
	case st[devicekey.KeyRevokedKey] != "":
		return "", devicekey.ErrRevoked
	case st[devicekey.KeyID] == "":
		return "", errors.New("this device isn't registered yet; start the agent with `logbook run`")
	case st[devicekey.KeyServer] != server:
		return "", fmt.Errorf("this device is registered with %s, not %s; start the agent with `logbook run` to register it",
			st[devicekey.KeyServer], server)
	}
	return st[devicekey.KeyID], nil
}

// submitHTTPClient is the agent's HTTP client with room for large uploads:
// the serverapi calls set their own deadlines (PlanTimeout,
// UploadTimeout), and the server may take a while to check a large body
// before it answers.
func submitHTTPClient() *http.Client {
	c := daemon.NewHTTPClient()
	c.Timeout = serverapi.UploadTimeout
	if t, ok := c.Transport.(*http.Transport); ok {
		t.ResponseHeaderTimeout = 5 * time.Minute
	}
	return c
}

// isTerminal reports whether f is an interactive terminal (a character
// device). /dev/null is one too; reading it gives end of input, which
// counts as no.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
