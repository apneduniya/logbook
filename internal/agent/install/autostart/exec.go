package autostart

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// commandTimeout bounds every launchctl, systemctl, pkill and taskkill call.
const commandTimeout = 15 * time.Second

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Spawner starts binPath with args as a detached background process that
// outlives the installer, without a console window.
type Spawner interface {
	Spawn(binPath string, args ...string) error
}

// ExecRunner runs real commands, each with a timeout.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers in this package pass fixed program names (launchctl, systemctl, pkill, taskkill)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// run calls r with a fresh timeout context. The public interface has no
// ctx (the install command's contract), so the timeout lives here. The
// output is already part of the error when the command fails.
func run(r Runner, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	_, err := r.Run(ctx, name, args...)
	return err
}

// exitCoder is satisfied by *exec.ExitError (and test fakes).
type exitCoder interface{ ExitCode() int }

// exitCode returns the process exit code inside err, or -1.
func exitCode(err error) int {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}
