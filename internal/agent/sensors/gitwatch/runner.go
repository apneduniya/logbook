package gitwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxOutput caps what one git process may write to stdout.
const maxOutput = 1 << 20

// ErrOutputTooLarge means git wrote more than maxOutput bytes.
var ErrOutputTooLarge = errors.New("git output too large")

// Runner runs git in a repository directory and returns its stdout.
type Runner interface {
	Git(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// ExecRunner runs the system git binary.
type ExecRunner struct {
	// Path is the git executable (from exec.LookPath).
	Path string
	// Timeout bounds each git process.
	Timeout time.Duration
}

// hardening is passed before every git subcommand.
var hardening = []string{
	"--no-pager",
	"-c", "core.fsmonitor=false", // never start or query a filesystem monitor
	"-c", "core.untrackedCache=false", // never rewrite the index's untracked cache
	"-c", "log.showSignature=false", // never run gpg
	"-c", "color.ui=false",
}

// Git runs `git <hardening> args...` in dir with a timeout. stdin is empty,
// so git can never wait for input.
func (r ExecRunner) Git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Path, append(append([]string{}, hardening...), args...)...) //nolint:gosec // fixed git binary and fixed arguments; dir is a repo path, not an argument
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	cmd.WaitDelay = time.Second // don't hang on pipes held by a stuck grandchild
	hideWindow(cmd)
	stdout := &capBuffer{max: maxOutput}
	stderr := &capBuffer{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("git %s: timed out after %v: %w", args[0], r.Timeout, ctx.Err())
	case stdout.overflow:
		return nil, fmt.Errorf("git %s: %w", args[0], ErrOutputTooLarge)
	case err != nil:
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// gitEnv returns env with the variables that make git safe and predictable
// for a background reader, replacing any inherited values.
func gitEnv(env []string) []string {
	set := map[string]string{
		"GIT_OPTIONAL_LOCKS":  "0", // don't take index.lock to refresh stat info
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "cat",
		"LC_ALL":              "C",
		"LANG":                "C",
		"LANGUAGE":            "C",
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := set[k]; override {
			continue
		}
		// Variables that would point git at another repo or work tree, or
		// make it run external programs.
		switch strings.ToUpper(k) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
			"GIT_EXTERNAL_DIFF", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND",
			"GIT_TRACE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT":
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

// capBuffer keeps at most max bytes and records whether more was written.
// It keeps accepting writes so the child never blocks on a full pipe.
type capBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
