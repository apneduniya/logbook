package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
	"github.com/apneduniya/logbook/internal/agent/localkey"
)

// envOf returns settings.json's "env" as a map (nil when absent).
func envOf(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc struct {
		Env map[string]any `json:"env"`
	}
	if err := json.Unmarshal(harnesstest.ReadFile(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Env
}

func install(t *testing.T, env hostenv.Env) (*Installer, string) {
	t.Helper()
	path, err := SettingsPath(env)
	if err != nil {
		t.Fatal(err)
	}
	in := NewInstaller(env)
	if err := in.Install(harnesstest.Bin); err != nil {
		t.Fatal(err)
	}
	return in, path
}

func TestTelemetryValues(t *testing.T) {
	env, _ := harnesstest.Env(t)
	in, path := install(t, env)
	tok, err := localkey.OTLPToken(hostenv.TestAgent.LocalToken)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
		"OTEL_METRICS_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://127.0.0.1:47231/v1/metrics",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "X-Logbook-Token=" + tok,
		"OTEL_METRIC_EXPORT_INTERVAL":         "60000",
	}
	got := envOf(t, path)
	if len(got) != len(want) {
		t.Fatalf("env = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env.%s = %v, want %v", k, got[k], v)
		}
	}
	// The local token itself never goes into settings.json.
	if strings.Contains(string(harnesstest.ReadFile(t, path)), hostenv.TestAgent.LocalToken) {
		t.Fatal("local token written to settings.json")
	}
	if n := in.Notes(); len(n) != 0 {
		t.Fatalf("notes = %v", n)
	}
}

// Notes explain every case where telemetry was left alone.
func TestTelemetryConflictNotes(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		getenv   map[string]string
		want     string
	}{
		{"user OTel endpoint", userOTel, nil, "env.CLAUDE_CODE_ENABLE_TELEMETRY"},
		{"other OTEL key", `{"env": {"OTEL_RESOURCE_ATTRIBUTES": "team=a"}}`, nil, "env.OTEL_RESOURCE_ATTRIBUTES"},
		{"headers helper", `{"otelHeadersHelper": "x"}`, nil, "otelHeadersHelper"},
		{"different exporter", `{"env": {"OTEL_METRICS_EXPORTER": "console"}}`, nil, "env.OTEL_METRICS_EXPORTER"},
		{"shell endpoint", "", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, "OTEL_EXPORTER_OTLP_ENDPOINT is set in your environment"},
		{"shell telemetry", "", map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1"}, "CLAUDE_CODE_ENABLE_TELEMETRY is set in your environment"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := harnesstest.Env(t)
			env.Getenv = func(k string) string { return tc.getenv[k] }
			path, _ := SettingsPath(env)
			if tc.existing != "" {
				harnesstest.WriteFile(t, path, tc.existing)
			}
			in, _ := install(t, env)
			notes := in.Notes()
			if len(notes) != 1 || !strings.Contains(notes[0], tc.want) {
				t.Fatalf("notes = %v, want one mentioning %q", notes, tc.want)
			}
			for k, v := range envOf(t, path) {
				if s, _ := v.(string); strings.Contains(s, "X-Logbook-Token") || strings.Contains(s, "/v1/metrics") {
					t.Fatalf("Logbook telemetry added despite a conflict: %s=%v", k, v)
				}
			}
		})
	}
}

// addEnv adds entries to settings.json's "env", as a user editing it would.
func addEnv(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	doc, err := jsonedit.ParseObject(harnesstest.ReadFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := doc.Get("env")
	envObj, err := jsonedit.ParseObject(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range slices.Sorted(maps.Keys(entries)) {
		v, _ := json.Marshal(entries[k])
		envObj.Set(k, v)
	}
	b, _ := envObj.MarshalJSON()
	doc.Set("env", b)
	out, _ := doc.Indented()
	harnesstest.WriteFile(t, path, string(out))
}

// userCollector is the user's own setup added after install: it relies on
// the enable flag and metrics exporter Logbook wrote.
var userCollector = map[string]string{
	"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel.example.com",
	"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=Bearer user-secret",
}

// A user who adds their own OpenTelemetry setup after installing keeps it
// working: the next install removes only the entries that point at the
// agent and hands the enable flag and exporter over to the user's setup.
// Once the user's setup is gone, a later install takes them back.
func TestTelemetryYieldsToLaterUserSetup(t *testing.T) {
	env, backups := harnesstest.Env(t)
	_, path := install(t, env)
	addEnv(t, path, userCollector)

	in, _ := install(t, env)
	got := envOf(t, path)
	want := map[string]any{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "https://otel.example.com",
		"OTEL_EXPORTER_OTLP_HEADERS":   "Authorization=Bearer user-secret",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	notes := in.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "left CLAUDE_CODE_ENABLE_TELEMETRY and OTEL_METRICS_EXPORTER in place") {
		t.Fatalf("notes = %v", notes)
	}
	// A second install with the user's setup still there changes nothing.
	before := harnesstest.ReadFile(t, path)
	install(t, env)
	if !bytes.Equal(before, harnesstest.ReadFile(t, path)) {
		t.Fatal("re-install changed settings.json")
	}
	// Uninstall leaves the handed-over entries: the user's setup uses them.
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, path); !maps.Equal(got, want) {
		t.Fatalf("env after uninstall = %v", got)
	}
	if _, err := os.Stat(filepath.Join(backups, Name+".telemetry.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ownership record left behind: %v", err)
	}
}

// Handed-over entries are taken back when the user's setup goes away.
func TestTelemetryTakesBackHandedOverEntries(t *testing.T) {
	env, _ := harnesstest.Env(t)
	_, path := install(t, env)
	addEnv(t, path, userCollector)
	install(t, env)
	// The user removes their own collector again.
	doc, _ := jsonedit.ParseObject(harnesstest.ReadFile(t, path))
	e, _ := doc.Get("env")
	envObj, _ := jsonedit.ParseObject(e)
	for k := range userCollector {
		envObj.Delete(k)
	}
	b, _ := envObj.MarshalJSON()
	doc.Set("env", b)
	out, _ := doc.Indented()
	harnesstest.WriteFile(t, path, string(out))

	in, _ := install(t, env)
	if got := envOf(t, path); len(got) != len(telemetryKeys) || len(in.Notes()) != 0 {
		t.Fatalf("env = %v, notes = %v", got, in.Notes())
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings.json Logbook created still exists: %v", err)
	}
}

// The daemon's re-check hands telemetry over to a setup added after
// install, without waiting for the next install, and never adds entries.
func TestYieldTelemetry(t *testing.T) {
	env, _ := harnesstest.Env(t)
	_, path := install(t, env)
	reason, err := YieldTelemetry(t.Context(), env)
	if err != nil || reason != "" {
		t.Fatalf("nothing to yield: reason %q, err %v", reason, err)
	}
	addEnv(t, path, userCollector)
	// The daemon's own environment is not the user's shell: ignored.
	env.Getenv = func(k string) string { return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x"}[k] }
	reason, err = YieldTelemetry(t.Context(), env)
	if err != nil || !strings.Contains(reason, "env.OTEL_EXPORTER_OTLP_") {
		t.Fatalf("reason %q, err %v", reason, err)
	}
	if got := envOf(t, path); len(got) != 4 || got["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != nil {
		t.Fatalf("env = %v", got)
	}
	if reason, err = YieldTelemetry(t.Context(), env); err != nil || reason != "" {
		t.Fatalf("second run: reason %q, err %v", reason, err)
	}

	// With nothing installed, it never adds anything.
	fresh, _ := harnesstest.Env(t)
	if reason, err := YieldTelemetry(t.Context(), fresh); err != nil || reason != "" {
		t.Fatalf("fresh: reason %q, err %v", reason, err)
	}
	if p, _ := SettingsPath(fresh); hostenv.Exists(p) {
		t.Fatal("YieldTelemetry created settings.json")
	}
}

// writeManaged writes a managed settings file for env's OS under its
// SystemRoot.
func writeManaged(t *testing.T, env hostenv.Env, name, body string) {
	t.Helper()
	dir := managedSystemDir(env.GOOS)
	harnesstest.WriteFile(t, env.SystemPath(joinSystem(env.GOOS, dir, name)), body)
}

// Managed settings that mention telemetry make Logbook stay out entirely:
// with a managed generic endpoint or credentials, Claude Code strips
// Logbook's per-signal endpoint but keeps its selectors, which would send
// the user's metrics to the administrator's collector.
func TestTelemetryYieldsToManagedSettings(t *testing.T) {
	logsOnly := `{"env": {"OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://corp.example.com",
		"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer corp"}}`
	tests := []struct {
		name, goos, file, body, want string
	}{
		{"linux file", "linux", "managed-settings.json", logsOnly, "managed-settings.json set env.OTEL_"},
		{"macOS file", "darwin", "managed-settings.json", `{"env": {"CLAUDE_CODE_ENABLE_TELEMETRY": "0"}}`, "env.CLAUDE_CODE_ENABLE_TELEMETRY"},
		{"windows file", "windows", "managed-settings.json", `{"otelHeadersHelper": "/opt/corp/otel.sh"}`, "otelHeadersHelper"},
		{"drop-in", "linux", filepath.Join("managed-settings.d", "10-telemetry.json"), logsOnly, "10-telemetry.json set env.OTEL_"},
		{"unparsable", "linux", "managed-settings.json", `{"env": `, "can't be parsed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := harnesstest.Env(t)
			env.GOOS = tc.goos
			path, _ := SettingsPath(env)
			harnesstest.WriteFile(t, path, userEnv)
			writeManaged(t, env, tc.file, tc.body)
			in, _ := install(t, env)
			notes := in.Notes()
			if len(notes) != 1 || !strings.Contains(notes[0], tc.want) || strings.Contains(notes[0], "left") {
				t.Fatalf("notes = %v, want one mentioning %q", notes, tc.want)
			}
			if got := envOf(t, path); len(got) != 2 {
				t.Fatalf("env = %v", got)
			}
		})
	}
}

// Managed settings without telemetry, and drop-ins that aren't JSON files,
// don't get in the way.
func TestTelemetryIgnoresUnrelatedManagedSettings(t *testing.T) {
	env, _ := harnesstest.Env(t)
	writeManaged(t, env, "managed-settings.json", `{"permissions": {"deny": ["Read(.env)"]}, "env": {"DISABLE_AUTOUPDATER": "1"}}`)
	writeManaged(t, env, filepath.Join("managed-settings.d", "README.md"), "OTEL_LOGS_EXPORTER")
	writeManaged(t, env, filepath.Join("managed-settings.d", ".hidden.json"), `{"env": {"OTEL_LOGS_EXPORTER": "otlp"}}`)
	in, path := install(t, env)
	if got := envOf(t, path); len(got) != len(telemetryKeys) || len(in.Notes()) != 0 {
		t.Fatalf("env = %v, notes = %v", got, in.Notes())
	}
}

// When an administrator adds telemetry to managed settings after install,
// the daemon's re-check removes every entry Logbook wrote, the selectors
// too: the user never opted into exporting to that collector.
func TestYieldTelemetryToLaterManagedSettings(t *testing.T) {
	env, _ := harnesstest.Env(t)
	_, path := install(t, env)
	harnesstest.WriteFile(t, filepath.Join(env.Home, ".claude", "remote-settings.json"),
		`{"env": {"OTEL_EXPORTER_OTLP_ENDPOINT": "https://corp.example.com"}}`)
	reason, err := YieldTelemetry(t.Context(), env)
	if err != nil || !strings.Contains(reason, "remote-settings.json") {
		t.Fatalf("reason %q, err %v", reason, err)
	}
	if got := envOf(t, path); got != nil {
		t.Fatalf("env = %v", got)
	}
}

// A changed local address (or token) updates Logbook's own entries.
func TestTelemetryUpdatesStaleEntries(t *testing.T) {
	env, _ := harnesstest.Env(t)
	_, path := install(t, env)
	env.Agent = func() (hostenv.Agent, error) {
		return hostenv.Agent{LocalAddr: "localhost:50000", LocalToken: strings.Repeat("f", 64)}, nil
	}
	in, _ := install(t, env)
	got := envOf(t, path)
	if got["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != "http://127.0.0.1:50000/v1/metrics" || len(in.Notes()) != 0 {
		t.Fatalf("env = %v, notes = %v", got, in.Notes())
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings.json Logbook created still exists: %v", err)
	}
}

// Uninstall removes exactly what Logbook added, only while it still holds
// Logbook's value; an entry the user edited, and the user's own entries,
// stay.
func TestTelemetryUninstallExactness(t *testing.T) {
	env, backups := harnesstest.Env(t)
	path, _ := SettingsPath(env)
	harnesstest.WriteFile(t, path, userEnv)
	in, _ := install(t, env)
	// The user changes the export interval after install.
	edited := strings.Replace(string(harnesstest.ReadFile(t, path)), `"60000"`, `"10000"`, 1)
	harnesstest.WriteFile(t, path, edited)
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	got := envOf(t, path)
	want := map[string]any{"BASH_DEFAULT_TIMEOUT_MS": "120000", "DISABLE_AUTOUPDATER": "1", "OTEL_METRIC_EXPORT_INTERVAL": "10000"}
	if len(got) != len(want) {
		t.Fatalf("env after uninstall = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env.%s = %v, want %v", k, got[k], v)
		}
	}
	if _, err := os.Stat(filepath.Join(backups, Name+".telemetry.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ownership record left behind: %v", err)
	}
}

// Without the ownership record (data dir wiped), uninstall removes nothing
// from "env": it can't tell Logbook's entries from the user's.
func TestTelemetryUninstallWithoutRecord(t *testing.T) {
	env, backups := harnesstest.Env(t)
	_, path := install(t, env)
	if err := os.Remove(filepath.Join(backups, Name+".telemetry.json")); err != nil {
		t.Fatal(err)
	}
	if err := uninstallTelemetry(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if len(envOf(t, path)) != len(telemetryKeys) {
		t.Fatalf("env = %v", envOf(t, path))
	}
}

// Without an agent config, install still adds the hooks and Notes says why
// telemetry is missing.
func TestTelemetryWithoutAgentConfig(t *testing.T) {
	env, _ := harnesstest.Env(t)
	env.Agent = func() (hostenv.Agent, error) { return hostenv.Agent{}, errors.New("not configured") }
	in, path := install(t, env)
	if envOf(t, path) != nil {
		t.Fatalf("env = %v", envOf(t, path))
	}
	if n := in.Notes(); len(n) != 1 || !strings.Contains(n[0], "not configured") {
		t.Fatalf("notes = %v", n)
	}
	env.Agent = func() (hostenv.Agent, error) {
		return hostenv.Agent{LocalAddr: "192.168.1.5:47231", LocalToken: strings.Repeat("a", 64)}, nil
	}
	in, path = install(t, env)
	if envOf(t, path) != nil || len(in.Notes()) != 1 {
		t.Fatalf("non-loopback address: env = %v, notes = %v", envOf(t, path), in.Notes())
	}
}

func TestPlanTelemetry(t *testing.T) {
	want := []kv{{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"}, {"OTEL_METRICS_EXPORTER", "otlp"}, {"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/v1/metrics"}}
	owned := ownership{Keys: map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_METRICS_EXPORTER": "otlp",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://127.0.0.1:1/v1/metrics"}}
	handed := ownership{HandedOver: map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_METRICS_EXPORTER": "otlp"}}
	const ours = `"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_METRICS_EXPORTER":"otlp","OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":"http://127.0.0.1:1/v1/metrics"`
	obj := func(s string) *jsonedit.Object {
		o, err := jsonedit.ParseObject([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	shellOTel := func(k string) string { return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x"}[k] }
	shellFlag := func(k string) string { return map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1"}[k] }
	all := []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"}
	selectors := []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_METRICS_EXPORTER"}
	endpoint := []string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"}
	tests := []struct {
		name     string
		in       planInput
		conflict bool
		remove   []string
		handover []string
	}{
		{name: "no env", in: planInput{}},
		{name: "unrelated env", in: planInput{env: obj(`{"FOO":"1"}`)}},
		{name: "ours", in: planInput{env: obj(`{` + ours + `}`), owned: owned}},
		{name: "handed over, user setup gone", in: planInput{env: obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_METRICS_EXPORTER":"otlp"}`), owned: handed}},
		{name: "same value but not ours", in: planInput{env: obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1"}`)}, conflict: true},
		{name: "non-string value", in: planInput{env: obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":1}`), owned: owned}, conflict: true},
		{
			name: "user OTEL key next to ours", in: planInput{env: obj(`{` + ours + `,"OTEL_LOGS_EXPORTER":"otlp"}`), owned: owned},
			conflict: true, remove: endpoint, handover: selectors,
		},
		{
			name: "user changed the exporter", in: planInput{env: obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_METRICS_EXPORTER":"console"}`), owned: owned},
			conflict: true, handover: selectors[:1],
		},
		{
			// A tuning entry isn't an exporter of the user's: nothing to hand over.
			name: "user resource attributes", in: planInput{env: obj(`{` + ours + `,"OTEL_RESOURCE_ATTRIBUTES":"team=a"}`), owned: owned},
			conflict: true, remove: all,
		},
		{
			name: "helper", in: planInput{env: obj(`{` + ours + `}`), owned: owned, hasHelper: true},
			conflict: true, remove: endpoint, handover: selectors,
		},
		{
			name: "shell OTLP endpoint", in: planInput{env: obj(`{` + ours + `}`), owned: owned, shell: shellOTel},
			conflict: true, remove: endpoint, handover: selectors,
		},
		{
			// Only the enable flag in the shell: no exporter of the user's
			// needs Logbook's selectors.
			name: "shell enable flag only", in: planInput{env: obj(`{` + ours + `}`), owned: owned, shell: shellFlag},
			conflict: true, remove: all,
		},
		{
			name: "managed telemetry removes everything", in: planInput{env: obj(`{` + ours + `,"OTEL_LOGS_EXPORTER":"otlp"}`), owned: owned, managed: "managed settings m set env.OTEL_EXPORTER_OTLP_ENDPOINT"},
			conflict: true, remove: all,
		},
		{
			name: "managed telemetry removes handed-over entries", in: planInput{env: obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_METRICS_EXPORTER":"otlp","OTEL_LOGS_EXPORTER":"otlp"}`), owned: handed, managed: "m"},
			conflict: true, remove: selectors,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.want = want
			p := planTelemetry(tc.in)
			if (p.conflict != "") != tc.conflict || !slices.Equal(p.remove, tc.remove) || !slices.Equal(p.handover, tc.handover) {
				t.Fatalf("plan = %+v", p)
			}
			if !tc.conflict && len(p.set) != len(want) {
				t.Fatalf("set = %v", p.set)
			}
		})
	}
}

func TestDocTelemetry(t *testing.T) {
	tests := []struct {
		name string
		doc  managedDoc
		want string
	}{
		{"empty", managedDoc{source: "m", data: []byte("  ")}, ""},
		{"no env", managedDoc{source: "m", data: []byte(`{"model":"opus"}`)}, ""},
		{"unrelated env", managedDoc{source: "m", data: []byte(`{"env":{"HTTPS_PROXY":"http://proxy"}}`)}, ""},
		{"BOM and OTEL", managedDoc{source: "m", data: []byte("\xef\xbb\xbf" + `{"env":{"OTEL_METRICS_EXPORTER":"none"}}`)}, "m set env.OTEL_METRICS_EXPORTER"},
		{"enable flag", managedDoc{source: "m", data: []byte(`{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1"}}`)}, "m set env.CLAUDE_CODE_ENABLE_TELEMETRY"},
		{"helper", managedDoc{source: "m", data: []byte(`{"otelHeadersHelper":"x"}`)}, `m set "otelHeadersHelper"`},
		{"env not object", managedDoc{source: "m", data: []byte(`{"env":[]}`)}, "isn't an object"},
		{"unreadable", managedDoc{source: "m", err: os.ErrPermission}, "m can't be read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := docTelemetry(tc.doc)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("docTelemetry = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSystemPath(t *testing.T) {
	env := hostenv.Env{SystemRoot: "root"}
	if got, want := env.SystemPath(`C:\Program Files\ClaudeCode\managed-settings.json`),
		filepath.Join("root", "C", "Program Files", "ClaudeCode", "managed-settings.json"); got != want {
		t.Fatalf("SystemPath = %q, want %q", got, want)
	}
	if got := (hostenv.Env{}).SystemPath("/etc/claude-code/x"); got != "/etc/claude-code/x" {
		t.Fatalf("real system path rewritten: %q", got)
	}
}
