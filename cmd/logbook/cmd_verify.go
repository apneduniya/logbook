package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/apneduniya/logbook/internal/agent/audit"
)

func verifyCommand() Command {
	return Command{
		Name:  "verify",
		Usage: "check a submission's verification bundle offline: verify [--key FINGERPRINT]... <bundle.json[.gz]>",
		Run: func(_ context.Context, args []string) error {
			return runVerify(args, os.Stdout)
		},
	}
}

// errVerifyFailed makes `logbook verify` exit non-zero when a check fails.
var errVerifyFailed = errors.New("the bundle failed verification")

const verifyUsage = "usage: logbook verify [--key FINGERPRINT]... <bundle.json or bundle.json.gz>"

// runVerify checks a bundle. Each --key is a device key fingerprint from
// outside the bundle (`logbook status` on the device); when any is given,
// every device's key in the bundle must be one of them. Flags may come
// before or after the file.
func runVerify(args []string, stdout io.Writer) error {
	var keys []string
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Func("key", "a device key fingerprint the bundle's keys must be among (repeatable)", func(v string) error {
		keys = append(keys, v)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, verifyUsage)
	}
	rest := flags.Args()
	if len(rest) == 0 || rest[0] == "" {
		return errors.New(verifyUsage)
	}
	path := rest[0]
	if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 {
		return errors.New(verifyUsage)
	}
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // the user names the file to check
	if err != nil {
		return fmt.Errorf("opening the bundle: %w", err)
	}
	defer func() { _ = f.Close() }() // opened read-only: closing can't lose data
	b, err := audit.Read(f)
	if err != nil {
		return err
	}
	rep := audit.Check(b)
	rep.PinKeys(keys)
	if err := rep.Write(stdout); err != nil {
		return err
	}
	if !rep.OK() {
		return errVerifyFailed
	}
	return nil
}
