package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
	"github.com/apneduniya/logbook/internal/agent/localkey"
)

// Telemetry setup: `logbook install` points Claude Code's OpenTelemetry
// metrics exporter at the daemon by adding entries to the "env" object of
// settings.json, so the agent receives claude_code.active_time.total (see
// package otlp). Only metrics are exported: logs and traces stay off
// because their exporter variables are never set.
//
// The user's own telemetry always wins. If settings.json, or the
// environment `logbook install` runs in, already has any OpenTelemetry
// setting (an OTEL_* variable, a different value for one of Logbook's keys,
// or an otelHeadersHelper), Logbook adds nothing, removes what it added
// earlier, and Notes explains why.
//
// Which entries Logbook added is recorded in a sidecar file next to the
// settings backup (<data dir>/backups/claude-code.telemetry.json). Uninstall
// removes exactly those entries, and only while they still hold the value
// Logbook wrote.

// telemetryKeys are the env entries Logbook adds, in the order it adds them.
var telemetryKeys = []string{
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"OTEL_METRICS_EXPORTER",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	"OTEL_METRIC_EXPORT_INTERVAL",
}

// shellKeys are variables that, set in the user's shell, mean they already
// export Claude Code telemetry somewhere (generic OTLP headers would also be
// merged into Logbook's exporter and sent to the agent).
var shellKeys = append([]string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_LOGS_EXPORTER",
	"OTEL_TRACES_EXPORTER",
}, telemetryKeys...)

// exportIntervalMS is how often Claude Code exports metrics (its default).
const exportIntervalMS = "60000"

// kv is one env entry.
type kv struct{ Key, Value string }

// wantTelemetry returns the env entries that point Claude Code's metrics at
// the agent. The header carries the OTLP token derived from the local token
// (never the local token itself: settings.json is often published with
// dotfiles).
func wantTelemetry(agent hostenv.Agent) ([]kv, error) {
	host, port, err := net.SplitHostPort(agent.LocalAddr)
	if err != nil {
		return nil, fmt.Errorf("local_addr %q: %w", agent.LocalAddr, err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("local_addr %q is not a loopback address", agent.LocalAddr)
	}
	tok, err := localkey.OTLPToken(agent.LocalToken)
	if err != nil {
		return nil, err
	}
	return []kv{
		{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"},
		{"OTEL_METRICS_EXPORTER", "otlp"},
		{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/json"},
		{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://" + net.JoinHostPort(host, port) + "/v1/metrics"},
		{"OTEL_EXPORTER_OTLP_METRICS_HEADERS", "X-Logbook-Token=" + tok},
		{"OTEL_METRIC_EXPORT_INTERVAL", exportIntervalMS},
	}, nil
}

// ownership is the sidecar: the entries Logbook added and whether it
// created the "env" object.
type ownership struct {
	EnvCreated bool              `json:"env_created"`
	Keys       map[string]string `json:"keys"`
}

// telemetryPlan is what install should do to "env".
type telemetryPlan struct {
	// conflict, when set, says why Logbook leaves telemetry to the user.
	conflict string
	// set are the entries to write (all of want, unless there's a conflict).
	set []kv
	// remove are entries Logbook owns that must go (on conflict).
	remove []string
}

// planTelemetry decides what install does, from the current "env" object
// (nil if absent), whether settings.json has an otelHeadersHelper, what
// Logbook owns, the wanted entries, and the installer's environment. It is
// pure.
func planTelemetry(env *jsonedit.Object, hasHelper bool, owned ownership, want []kv, getenv func(string) string) telemetryPlan {
	ours := map[string]string{}
	for _, w := range want {
		ours[w.Key] = w.Value
	}
	conflict := ""
	switch {
	case hasHelper:
		conflict = `settings.json has "otelHeadersHelper"`
	default:
		for _, k := range shellKeys {
			if getenv(k) != "" {
				conflict = k + " is set in your environment"
				break
			}
		}
	}
	if conflict == "" && env != nil {
		for _, k := range env.Keys() {
			raw, _ := env.Get(k)
			cur, isString := stringValue(raw)
			_, mine := ours[k]
			switch {
			case mine && isString && owned.Keys[k] != "" && (cur == ours[k] || cur == owned.Keys[k]):
				// already Logbook's, current or stale
			case mine:
				conflict = `settings.json already sets env.` + k
			case strings.HasPrefix(k, "OTEL_"):
				conflict = `settings.json already sets env.` + k
			}
			if conflict != "" {
				break
			}
		}
	}
	if conflict != "" {
		return telemetryPlan{conflict: conflict, remove: ownedPresent(env, owned)}
	}
	return telemetryPlan{set: want}
}

// ownedPresent lists the owned entries still holding the value Logbook
// wrote, in a fixed order.
func ownedPresent(env *jsonedit.Object, owned ownership) []string {
	if env == nil {
		return nil
	}
	var out []string
	for _, k := range telemetryKeys {
		want, ok := owned.Keys[k]
		if !ok {
			continue
		}
		raw, present := env.Get(k)
		if cur, isString := stringValue(raw); present && isString && cur == want {
			out = append(out, k)
		}
	}
	return out
}

func stringValue(raw json.RawMessage) (string, bool) {
	var s string
	if raw == nil || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// settingsFile is settings.json as read for a telemetry change.
type settingsFile struct {
	path     string
	original []byte
	existed  bool
	mode     fs.FileMode
	doc      *jsonedit.Object
	env      *jsonedit.Object // nil when there is no "env" object
}

// errEnvNotObject means "env" exists but isn't an object; Logbook leaves it.
var errEnvNotObject = errors.New(`"env" is not an object`)

func readSettings(env hostenv.Env) (settingsFile, error) {
	p, err := SettingsPath(env)
	if err != nil {
		return settingsFile{}, err
	}
	path, err := hookfile.Resolve(p)
	if err != nil {
		return settingsFile{}, err
	}
	data, existed, mode, err := hookfile.ReadFile(path)
	if err != nil {
		return settingsFile{}, err
	}
	if !existed {
		mode = 0o600
	}
	f := settingsFile{path: path, original: data, existed: existed, mode: mode, doc: jsonedit.NewObject()}
	body := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(body)) > 0 {
		if f.doc, err = jsonedit.ParseObject(body); err != nil {
			return settingsFile{}, fmt.Errorf("%s is not valid JSON, leaving it alone: %w", path, err)
		}
	}
	if raw, ok := f.doc.Get("env"); ok {
		if f.env, err = jsonedit.ParseObject(raw); err != nil {
			return settingsFile{}, fmt.Errorf("%s: %w", path, errEnvNotObject)
		}
	}
	return f, nil
}

// sidecarPath is where the ownership record lives.
func sidecarPath(env hostenv.Env) (string, error) {
	dir, err := env.BackupDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, Name+".telemetry.json"), nil
}

func readOwnership(env hostenv.Env) (ownership, error) {
	path, err := sidecarPath(env)
	if err != nil {
		return ownership{}, err
	}
	data, existed, _, err := hookfile.ReadFile(path)
	if err != nil || !existed {
		return ownership{}, err
	}
	var o ownership
	if err := json.Unmarshal(data, &o); err != nil {
		return ownership{}, nil //nolint:nilerr // a damaged record means Logbook owns nothing; uninstall then removes nothing
	}
	return o, nil
}

func writeOwnership(env hostenv.Env, o ownership) error {
	path, err := sidecarPath(env)
	if err != nil {
		return err
	}
	if len(o.Keys) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
		return nil
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding telemetry record: %w", err)
	}
	return hookfile.WriteAtomic(path, append(b, '\n'), 0o600)
}

// telemetryState is everything install, uninstall and Notes look at.
type telemetryState struct {
	file  settingsFile
	owned ownership
	plan  telemetryPlan
}

// loadTelemetry reads settings.json and the sidecar and plans the change.
func loadTelemetry(env hostenv.Env) (telemetryState, error) {
	if env.Agent == nil {
		return telemetryState{}, errors.New("agent config unavailable")
	}
	agent, err := env.Agent()
	if err != nil {
		return telemetryState{}, err
	}
	want, err := wantTelemetry(agent)
	if err != nil {
		return telemetryState{}, err
	}
	f, err := readSettings(env)
	if err != nil {
		return telemetryState{}, err
	}
	owned, err := readOwnership(env)
	if err != nil {
		return telemetryState{}, err
	}
	_, hasHelper := f.doc.Get("otelHeadersHelper")
	return telemetryState{file: f, owned: owned, plan: planTelemetry(f.env, hasHelper, owned, want, env.Getenv)}, nil
}

// installTelemetry applies the plan. A conflict is not an error: the user's
// own setup is left as it is and Notes explains.
func installTelemetry(env hostenv.Env) error {
	st, err := loadTelemetry(env)
	if err != nil {
		return nil //nolint:nilerr // telemetry is optional; Notes reports why it wasn't set up
	}
	f, owned := st.file, st.owned
	envObj := f.env
	created := false
	if envObj == nil {
		if len(st.plan.set) == 0 {
			return nil
		}
		envObj, created = jsonedit.NewObject(), true
	}
	changed := false
	next := ownership{EnvCreated: owned.EnvCreated || created, Keys: map[string]string{}}
	for _, k := range st.plan.remove {
		envObj.Delete(k)
		changed = true
	}
	for _, e := range st.plan.set {
		raw, _ := json.Marshal(e.Value) // marshalling a string can't fail
		if cur, ok := envObj.Get(e.Key); !ok || !jsonedit.Equal(cur, raw) {
			envObj.Set(e.Key, raw)
			changed = true
		}
		next.Keys[e.Key] = e.Value
	}
	if !changed {
		return nil
	}
	return saveTelemetry(env, f, envObj, next)
}

// uninstallTelemetry removes the entries Logbook added that still hold the
// value it wrote, and "env" itself if Logbook created it and it is now empty.
func uninstallTelemetry(env hostenv.Env) error {
	f, err := readSettings(env)
	if errors.Is(err, errEnvNotObject) {
		// The user replaced "env"; nothing of Logbook's can be in it.
		return writeOwnership(env, ownership{})
	}
	if err != nil {
		return err
	}
	owned, err := readOwnership(env)
	if err != nil {
		return err
	}
	remove := ownedPresent(f.env, owned)
	if len(remove) == 0 {
		return writeOwnership(env, ownership{})
	}
	for _, k := range remove {
		f.env.Delete(k)
	}
	return saveTelemetry(env, f, f.env, ownership{EnvCreated: owned.EnvCreated})
}

// saveTelemetry writes settings.json with envObj as "env" (removed when it
// is empty and Logbook created it), after backing up the original once, and
// records what Logbook owns.
func saveTelemetry(env hostenv.Env, f settingsFile, envObj *jsonedit.Object, owned ownership) error {
	if envObj.Len() == 0 && owned.EnvCreated {
		f.doc.Delete("env")
		owned.EnvCreated = false
	} else {
		raw, err := envObj.MarshalJSON()
		if err != nil {
			return fmt.Errorf("encoding env: %w", err)
		}
		f.doc.Set("env", raw)
	}
	dir, err := env.BackupDir()
	if err != nil {
		return err
	}
	if err := hookfile.BackupOnce(dir, Name, f.path, f.original, f.existed); err != nil {
		return err
	}
	// Record ownership first: if writing settings.json fails, the record
	// lists values the file doesn't hold, which uninstall then leaves alone.
	if err := writeOwnership(env, owned); err != nil {
		return err
	}
	out, err := f.doc.Indented()
	if err != nil {
		return fmt.Errorf("encoding %s: %w", f.path, err)
	}
	return hookfile.WriteAtomic(f.path, out, f.mode)
}

// telemetryNotes explains, after install, why telemetry wasn't set up.
func telemetryNotes(env hostenv.Env) []string {
	st, err := loadTelemetry(env)
	switch {
	case err != nil:
		return []string{"Claude Code active time is not recorded: " + err.Error()}
	case st.plan.conflict != "":
		return []string{"Claude Code telemetry was left as you set it up (" + st.plan.conflict + "), so Logbook " +
			"doesn't receive Claude Code's own active-time measure. Hooks still record your sessions."}
	}
	return nil
}
