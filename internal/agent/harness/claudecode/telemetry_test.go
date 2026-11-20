package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// A user who adds their own OpenTelemetry setup after installing gets
// Logbook's entries removed on the next install; theirs stay.
func TestTelemetryYieldsToLaterUserSetup(t *testing.T) {
	env, _ := harnesstest.Env(t)
	_, path := install(t, env)
	raw := harnesstest.ReadFile(t, path)
	doc, err := jsonedit.ParseObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := doc.Get("env")
	envObj, _ := jsonedit.ParseObject(e)
	envObj.Set("OTEL_EXPORTER_OTLP_ENDPOINT", json.RawMessage(`"https://otel.example.com"`))
	b, _ := envObj.MarshalJSON()
	doc.Set("env", b)
	out, _ := doc.Indented()
	harnesstest.WriteFile(t, path, string(out))

	in, _ := install(t, env)
	got := envOf(t, path)
	if len(got) != 1 || got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://otel.example.com" {
		t.Fatalf("env = %v", got)
	}
	if len(in.Notes()) != 1 {
		t.Fatal("no note about the user's setup")
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
	if err := uninstallTelemetry(env); err != nil {
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
	want := []kv{{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"}, {"OTEL_METRICS_EXPORTER", "otlp"}}
	owned := ownership{Keys: map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_METRICS_EXPORTER": "otlp"}}
	obj := func(s string) *jsonedit.Object {
		o, err := jsonedit.ParseObject([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	none := func(string) string { return "" }
	tests := []struct {
		name     string
		env      *jsonedit.Object
		owned    ownership
		conflict bool
		remove   int
	}{
		{"no env", nil, ownership{}, false, 0},
		{"unrelated env", obj(`{"FOO":"1"}`), ownership{}, false, 0},
		{"ours", obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_METRICS_EXPORTER":"otlp"}`), owned, false, 0},
		{"same value but not ours", obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1"}`), ownership{}, true, 0},
		{"non-string value", obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":1}`), owned, true, 0},
		{"user OTEL key next to ours", obj(`{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_LOGS_EXPORTER":"otlp"}`), owned, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := planTelemetry(tc.env, false, tc.owned, want, none)
			if (p.conflict != "") != tc.conflict || len(p.remove) != tc.remove {
				t.Fatalf("plan = %+v", p)
			}
			if !tc.conflict && len(p.set) != len(want) {
				t.Fatalf("set = %v", p.set)
			}
		})
	}
}
