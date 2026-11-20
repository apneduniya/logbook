package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
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
// Anyone else's telemetry setup always wins. Logbook adds nothing, and
// removes what it added earlier, when:
//   - managed settings mention telemetry at all (managed.go explains why
//     that matters for privacy); or
//   - settings.json, or the environment `logbook install` runs in, has any
//     OpenTelemetry setting of the user's (an OTEL_* variable, a different
//     value for one of Logbook's keys, or an otelHeadersHelper).
//
// When it yields to the user's own setup (not to managed settings), the
// two entries that switch export on, CLAUDE_CODE_ENABLE_TELEMETRY and
// OTEL_METRICS_EXPORTER, are handed over instead of removed: the user's
// setup may have been relying on them since they added it. Only the
// entries that point at the agent (endpoint, headers, protocol, interval)
// go. Notes says which entries were left.
//
// Install checks this once; the daemon re-checks it with YieldTelemetry,
// so a setup added later isn't overridden until the next install.
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

// selectorKeys are the entries that switch metrics export on, rather than
// say where it goes. They are handed over, not removed, when Logbook yields
// to the user's own setup.
var selectorKeys = []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_METRICS_EXPORTER"}

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
	// HandedOver are selector entries Logbook wrote and then left in place
	// for the user's own setup (key → the value Logbook wrote). They are no
	// longer Logbook's to remove, but a later install takes them back once
	// the user's setup is gone and they still hold that value.
	HandedOver map[string]string `json:"handed_over,omitempty"`
}

// wrote returns the value Logbook wrote for k, if it did (owned or handed
// over).
func (o ownership) wrote(k string) (string, bool) {
	if v, ok := o.Keys[k]; ok {
		return v, true
	}
	v, ok := o.HandedOver[k]
	return v, ok
}

func sameOwnership(a, b ownership) bool {
	return a.EnvCreated == b.EnvCreated && maps.Equal(a.Keys, b.Keys) && maps.Equal(a.HandedOver, b.HandedOver)
}

// telemetryPlan is what install should do to "env".
type telemetryPlan struct {
	// conflict, when set, says why Logbook leaves telemetry to someone else.
	conflict string
	// set are the entries to write (all of want, unless there's a conflict).
	set []kv
	// remove are entries Logbook wrote that must go (on conflict).
	remove []string
	// handover are selector entries Logbook wrote that stay for the user's
	// own setup (on a conflict with it).
	handover []string
}

// planInput is everything planTelemetry looks at.
type planInput struct {
	// env is settings.json's "env" object (nil if absent).
	env *jsonedit.Object
	// hasHelper is whether settings.json has an otelHeadersHelper.
	hasHelper bool
	owned     ownership
	// want are the entries Logbook would write (nil when unknown).
	want []kv
	// shell reads the environment Claude Code is likely to start from; nil
	// skips it (the daemon's environment isn't the user's shell).
	shell func(string) string
	// managed says why managed settings decide telemetry ("" if they don't).
	managed string
}

// planTelemetry decides what install does. It is pure.
func planTelemetry(in planInput) telemetryPlan {
	ours := map[string]string{}
	for _, w := range in.want {
		ours[w.Key] = w.Value
	}
	conflict := in.managed
	if conflict == "" {
		conflict = userConflict(in, ours)
	}
	if conflict == "" {
		return telemetryPlan{set: in.want}
	}
	// Under managed settings, Logbook's selectors would send metrics to the
	// administrator's collector: everything goes.
	handover := in.managed == "" && userOwnsOTel(in, ours)
	remove, kept := yieldEntries(in.env, in.owned, handover)
	return telemetryPlan{conflict: conflict, remove: remove, handover: kept}
}

// userConflict says why the user's own settings or shell conflict with
// Logbook's entries ("" if they don't).
func userConflict(in planInput, ours map[string]string) string {
	if in.hasHelper {
		return `settings.json has "otelHeadersHelper"`
	}
	if in.shell != nil {
		for _, k := range shellKeys {
			if in.shell(k) != "" {
				return k + " is set in your environment"
			}
		}
	}
	if in.env == nil {
		return ""
	}
	for _, k := range in.env.Keys() {
		_, mine := ours[k]
		if (mine || strings.HasPrefix(k, "OTEL_")) && !isLogbooks(in.env, in.owned, ours, k) {
			return `settings.json already sets env.` + k
		}
	}
	return ""
}

// userOwnsOTel reports whether the user has an exporter of their own that
// may rely on Logbook's selectors: an otelHeadersHelper, an OTLP variable
// in the shell, or an exporter, endpoint or headers entry in "env" that
// Logbook didn't write. Tuning entries alone (an export interval the user
// edited, resource attributes) don't send telemetry anywhere, so they don't
// count.
func userOwnsOTel(in planInput, ours map[string]string) bool {
	if in.hasHelper {
		return true
	}
	if in.shell != nil {
		for _, k := range shellKeys {
			if strings.HasPrefix(k, "OTEL_") && in.shell(k) != "" {
				return true
			}
		}
	}
	if in.env == nil {
		return false
	}
	for _, k := range in.env.Keys() {
		if isExporterKey(k) && !isLogbooks(in.env, in.owned, ours, k) {
			return true
		}
	}
	return false
}

// isExporterKey reports whether an env key chooses an OpenTelemetry
// exporter or where it sends (selectors, endpoints, headers, client
// certificates), generic or per signal.
func isExporterKey(k string) bool {
	if !strings.HasPrefix(k, "OTEL_") {
		return false
	}
	return strings.HasSuffix(k, "_EXPORTER") || strings.HasPrefix(k, "OTEL_EXPORTER_OTLP_") &&
		(strings.HasSuffix(k, "_ENDPOINT") || strings.HasSuffix(k, "_HEADERS") ||
			strings.HasSuffix(k, "_CLIENT_KEY") || strings.HasSuffix(k, "_CLIENT_CERTIFICATE"))
}

// isLogbooks reports whether env.k holds a value Logbook wrote (as recorded
// in the sidecar), or the value it would write now for a key it wrote
// before with another value (a stale entry after a config change).
func isLogbooks(env *jsonedit.Object, owned ownership, ours map[string]string, k string) bool {
	wrote, ok := owned.wrote(k)
	if !ok {
		return false
	}
	raw, _ := env.Get(k)
	cur, isString := stringValue(raw)
	return isString && (cur == wrote || (ours[k] != "" && cur == ours[k]))
}

// yieldEntries splits the entries Logbook wrote that still hold its value
// into those to remove and, when handover is set, the selectors to leave for
// the user. Both are in a fixed order.
func yieldEntries(env *jsonedit.Object, owned ownership, handover bool) (remove, kept []string) {
	for _, k := range writtenPresent(env, owned) {
		if handover && slices.Contains(selectorKeys, k) {
			kept = append(kept, k)
		} else {
			remove = append(remove, k)
		}
	}
	return remove, kept
}

// writtenPresent lists the entries Logbook wrote (owned or handed over)
// that still hold the value it wrote, in a fixed order.
func writtenPresent(env *jsonedit.Object, owned ownership) []string {
	if env == nil {
		return nil
	}
	var out []string
	for _, k := range telemetryKeys {
		want, ok := owned.wrote(k)
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
	if len(o.Keys) == 0 && len(o.HandedOver) == 0 {
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

// loadTelemetry reads settings.json, the sidecar and the managed settings
// and plans the change. shell is the environment to check for the user's
// own OTLP variables (nil skips it).
func loadTelemetry(ctx context.Context, env hostenv.Env, shell func(string) string) (telemetryState, error) {
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
	plan := planTelemetry(planInput{
		env: f.env, hasHelper: hasHelper, owned: owned, want: want, shell: shell,
		managed: managedTelemetry(managedDocs(ctx, env)),
	})
	return telemetryState{file: f, owned: owned, plan: plan}, nil
}

// installTelemetry applies the plan. A conflict is not an error: the other
// setup is left as it is and Notes explains.
func installTelemetry(ctx context.Context, env hostenv.Env) error {
	st, err := loadTelemetry(ctx, env, env.Getenv)
	if err != nil {
		return nil //nolint:nilerr // telemetry is optional; Notes reports why it wasn't set up
	}
	return applyPlan(env, st)
}

// YieldTelemetry hands Claude Code telemetry back to a setup that appeared
// after install. When managed settings, or the user's own entries in
// settings.json, now configure OpenTelemetry, it removes Logbook's exporter
// entries exactly as install would (handing the selectors over to a user's
// setup) and returns why. It never adds entries, and it doesn't look at the
// calling process's environment: the daemon calls it, and its environment
// isn't the user's shell. It returns "" when there was nothing to remove.
func YieldTelemetry(ctx context.Context, env hostenv.Env) (string, error) {
	st, err := loadTelemetry(ctx, env, nil)
	if err != nil {
		return "", err
	}
	if st.plan.conflict == "" || len(st.plan.remove) == 0 {
		return "", nil
	}
	if err := applyPlan(env, st); err != nil {
		return "", err
	}
	return st.plan.conflict, nil
}

// applyPlan writes the planned entries (or removes and hands over, on a
// conflict) and records what Logbook now owns.
func applyPlan(env hostenv.Env, st telemetryState) error {
	f, plan := st.file, st.plan
	next := ownership{EnvCreated: st.owned.EnvCreated, Keys: map[string]string{}, HandedOver: map[string]string{}}
	envObj := f.env
	if envObj == nil {
		if len(plan.set) == 0 {
			// Nothing of Logbook's can be in a missing "env".
			if sameOwnership(st.owned, ownership{}) {
				return nil
			}
			return writeOwnership(env, ownership{})
		}
		envObj, next.EnvCreated = jsonedit.NewObject(), true
	}
	changed := false
	for _, k := range plan.remove {
		envObj.Delete(k)
		changed = true
	}
	for _, k := range plan.handover {
		next.HandedOver[k], _ = st.owned.wrote(k)
	}
	for _, e := range plan.set {
		raw, _ := json.Marshal(e.Value) // marshalling a string can't fail
		if cur, ok := envObj.Get(e.Key); !ok || !jsonedit.Equal(cur, raw) {
			envObj.Set(e.Key, raw)
			changed = true
		}
		next.Keys[e.Key] = e.Value
	}
	if !changed {
		if sameOwnership(st.owned, next) {
			return nil
		}
		return writeOwnership(env, next)
	}
	return saveTelemetry(env, f, envObj, next)
}

// uninstallTelemetry removes the entries Logbook wrote that still hold its
// value, and "env" itself if Logbook created it and it is now empty. The
// selectors stay when the user has an OpenTelemetry setup of their own that
// may rely on them (never under managed telemetry settings).
func uninstallTelemetry(ctx context.Context, env hostenv.Env) error {
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
	_, hasHelper := f.doc.Get("otelHeadersHelper")
	in := planInput{env: f.env, hasHelper: hasHelper, owned: owned, shell: env.Getenv,
		managed: managedTelemetry(managedDocs(ctx, env))}
	remove, _ := yieldEntries(f.env, owned, in.managed == "" && userOwnsOTel(in, nil))
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
func telemetryNotes(ctx context.Context, env hostenv.Env) []string {
	st, err := loadTelemetry(ctx, env, env.Getenv)
	switch {
	case err != nil:
		return []string{"Claude Code active time is not recorded: " + err.Error()}
	case st.plan.conflict != "":
		note := "Claude Code telemetry is set up elsewhere (" + st.plan.conflict + "), so Logbook doesn't add its " +
			"exporter and doesn't receive Claude Code's own active-time measure. Hooks still record your sessions."
		if len(st.plan.handover) > 0 {
			note += " Logbook left " + strings.Join(st.plan.handover, " and ") + " in place because your setup " +
				"may rely on them; delete them if you don't want Claude Code to export metrics."
		}
		return []string{note}
	}
	return nil
}
