package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/apneduniya/logbook/internal/agent/audit"
)

func verifyCommand() Command {
	return Command{
		Name:  "verify",
		Usage: "check a submission's verification bundle offline: verify <bundle.json[.gz]>",
		Run: func(_ context.Context, args []string) error {
			return runVerify(args, os.Stdout)
		},
	}
}

// errVerifyFailed makes `logbook verify` exit non-zero when a check fails.
var errVerifyFailed = errors.New("the bundle failed verification")

func runVerify(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' {
		return errors.New("usage: logbook verify <bundle.json or bundle.json.gz>")
	}
	f, err := os.Open(filepath.Clean(args[0])) //nolint:gosec // the user names the file to check
	if err != nil {
		return fmt.Errorf("opening the bundle: %w", err)
	}
	defer func() { _ = f.Close() }() // opened read-only: closing can't lose data
	b, err := audit.Read(f)
	if err != nil {
		return err
	}
	rep := audit.Check(b)
	if err := rep.Write(stdout); err != nil {
		return err
	}
	if !rep.OK() {
		return errVerifyFailed
	}
	return nil
}
