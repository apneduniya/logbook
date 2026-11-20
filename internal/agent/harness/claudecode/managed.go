package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
)

// Managed settings: an administrator's policy, which Claude Code applies
// above the user's settings.json. When managed settings set a generic OTLP
// endpoint or credential, Claude Code removes developer-set per-signal
// endpoints and headers (Logbook's) but leaves exporter selectors to normal
// precedence (monitoring docs, "How managed settings lock the OTLP
// destination", read 2026-11-20). Logbook's CLAUDE_CODE_ENABLE_TELEMETRY
// and OTEL_METRICS_EXPORTER would then turn on metrics that go to the
// administrator's collector, with the user's email and account ids. So any
// telemetry entry in any managed source means Logbook adds nothing and
// removes everything it added.
//
// The sources, as listed in Claude Code's managed-settings docs (read
// 2026-11-20):
//   - managed-settings.json and managed-settings.d/*.json in the system
//     directory (/etc/claude-code, /Library/Application Support/ClaudeCode,
//     C:\Program Files\ClaudeCode);
//   - the server-managed settings cache, remote-settings.json in Claude
//     Code's config directory;
//   - macOS managed preferences (com.anthropic.claudecode), converted with
//     /usr/bin/plutil;
//   - the Windows registry value Settings under HKLM and HKCU
//     SOFTWARE\Policies\ClaudeCode (managed_windows.go).
//
// A source that exists but can't be read or parsed counts as a conflict:
// Claude Code itself refuses to start on a malformed managed file, and
// Logbook can't tell what it would decide.

// managedDoc is one managed source: where it came from and its content.
type managedDoc struct {
	source string
	data   []byte // JSON object; nil when err is set
	err    error
}

// managedSystemDir is the system directory of file-based managed settings.
func managedSystemDir(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	default: // Linux and WSL
		return "/etc/claude-code"
	}
}

// joinSystem joins a file name onto a system directory with the target
// OS's separator, so a Windows path stays one string on any test host.
func joinSystem(goos, dir, name string) string {
	if goos == "windows" {
		return dir + `\` + name
	}
	return dir + "/" + name
}

// macManagedPrefs is the macOS managed preferences file for Claude Code's
// domain; MDM writes a machine-wide one and may write a per-user one.
const macManagedPrefs = "/Library/Managed Preferences"

const macPrefsDomain = "com.anthropic.claudecode.plist"

// managedDocs reads every managed source that exists. The result is in a
// fixed order: system file, drop-ins (alphabetical), server cache, OS
// policies.
func managedDocs(ctx context.Context, env hostenv.Env) []managedDoc {
	var out []managedDoc
	dir := managedSystemDir(env.GOOS)
	out = appendFile(out, env.SystemPath(joinSystem(env.GOOS, dir, "managed-settings.json")))
	out = append(out, dropIns(env.SystemPath(joinSystem(env.GOOS, dir, "managed-settings.d")))...)
	if cfg, err := env.DirFromEnv("CLAUDE_CONFIG_DIR", ".claude"); err == nil {
		out = appendFile(out, filepath.Join(cfg, "remote-settings.json"))
	}
	if env.GOOS == "darwin" {
		out = appendPlist(ctx, out, env.SystemPath(macManagedPrefs+"/"+macPrefsDomain))
		if user := env.Getenv("USER"); user != "" && !strings.ContainsAny(user, `/\`) && user != ".." {
			out = appendPlist(ctx, out, env.SystemPath(macManagedPrefs+"/"+user+"/"+macPrefsDomain))
		}
	}
	if env.GOOS == "windows" && env.ReadsOSPolicy() {
		out = append(out, registryPolicies()...)
	}
	return out
}

// appendFile adds path if it exists.
func appendFile(out []managedDoc, path string) []managedDoc {
	data, existed, _, err := hookfile.ReadFile(path)
	switch {
	case err != nil:
		return append(out, managedDoc{source: path, err: err})
	case !existed:
		return out
	}
	return append(out, managedDoc{source: path, data: data})
}

// dropIns reads the *.json files of a managed-settings.d directory, in the
// alphabetical order Claude Code merges them in, skipping hidden files.
func dropIns(dir string) []managedDoc {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return []managedDoc{{source: dir, err: err}}
	}
	var out []managedDoc
	for _, e := range entries { // os.ReadDir sorts by name
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		out = appendFile(out, filepath.Join(dir, name))
	}
	return out
}

// plutilTimeout bounds the plist conversion.
const plutilTimeout = 5 * time.Second

// appendPlist adds a macOS managed preferences file if it exists, converted
// to JSON by the system's plutil (managed profiles are usually binary
// plists, and a parser would be a new dependency).
func appendPlist(ctx context.Context, out []managedDoc, path string) []managedDoc {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, plutilTimeout)
	defer cancel()
	var stdout bytes.Buffer
	//nolint:gosec // a fixed system tool; path is the fixed managed-preferences file
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return append(out, managedDoc{source: path, err: fmt.Errorf("converting with plutil: %w", err)})
	}
	return append(out, managedDoc{source: path, data: stdout.Bytes()})
}

// managedTelemetry returns why managed settings decide Claude Code's
// telemetry, or "" when no managed source mentions it.
func managedTelemetry(docs []managedDoc) string {
	for _, d := range docs {
		if reason := docTelemetry(d); reason != "" {
			return reason
		}
	}
	return ""
}

// docTelemetry checks one managed source: an otelHeadersHelper, or an env
// entry for OpenTelemetry (OTEL_*) or CLAUDE_CODE_ENABLE_TELEMETRY.
func docTelemetry(d managedDoc) string {
	if d.err != nil {
		return "managed settings " + d.source + " can't be read (" + d.err.Error() + ")"
	}
	body := bytes.TrimPrefix(d.data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	doc, err := jsonedit.ParseObject(body)
	if err != nil {
		return "managed settings " + d.source + " can't be parsed"
	}
	if _, ok := doc.Get("otelHeadersHelper"); ok {
		return "managed settings " + d.source + ` set "otelHeadersHelper"`
	}
	raw, ok := doc.Get("env")
	if !ok {
		return ""
	}
	envObj, err := jsonedit.ParseObject(raw)
	if err != nil {
		return "managed settings " + d.source + ` have an "env" that isn't an object`
	}
	keys := envObj.Keys()
	if i := slices.IndexFunc(keys, isTelemetryKey); i >= 0 {
		return "managed settings " + d.source + " set env." + keys[i]
	}
	return ""
}

// isTelemetryKey reports whether an env key configures Claude Code's
// OpenTelemetry export.
func isTelemetryKey(k string) bool {
	return strings.HasPrefix(k, "OTEL_") || k == "CLAUDE_CODE_ENABLE_TELEMETRY"
}
