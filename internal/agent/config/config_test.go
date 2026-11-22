package config

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Load() on empty dir err = %v, want ErrNotConfigured", err)
	}
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	c.APIKey = "00000000-0000-4000-8000-000000000000"
	if err := Save(dir, c); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got != c {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, c)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(Path(dir))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestValidate(t *testing.T) {
	ok, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad url", func(c *Config) { c.ServerURL = "not a url" }},
		{"ftp url", func(c *Config) { c.ServerURL = "ftp://x" }},
		{"short token", func(c *Config) { c.LocalToken = "abc" }},
		{"no addr", func(c *Config) { c.LocalAddr = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ok
			tt.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

// TestLoadMovesRetiredServer: configs saved while the default server was the
// web UI's origin move to the API's origin; custom servers are kept.
func TestLoadMovesRetiredServer(t *testing.T) {
	for _, tt := range []struct{ saved, want string }{
		{"https://logbook.boogles.dev", DefaultServerURL},
		{"https://logbook.boogles.dev/", DefaultServerURL},
		{"https://logbook.example.org", "https://logbook.example.org"},
		{"http://logbook.boogles.dev", "http://logbook.boogles.dev"}, // not the retired https origin
	} {
		dir := t.TempDir()
		c, err := Default()
		if err != nil {
			t.Fatal(err)
		}
		c.ServerURL = tt.saved
		if err := Save(dir, c); err != nil {
			t.Fatal(err)
		}
		got, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.ServerURL != tt.want {
			t.Errorf("saved %q: loaded %q, want %q", tt.saved, got.ServerURL, tt.want)
		}
	}
}

func TestMigrateRetired(t *testing.T) {
	for in, want := range map[string]string{
		"https://logbook.boogles.dev/api/v1":     "https://api.logbook.boogles.dev/api/v1",
		"https://Logbook.Boogles.dev/api/v1/":    "https://api.logbook.boogles.dev/api/v1",
		"https://logbook.boogles.dev:443/api/v1": "https://api.logbook.boogles.dev/api/v1",
		"HTTPS://logbook.boogles.dev/api/v1":     "https://api.logbook.boogles.dev/api/v1",
		"https://logbook.boogles.dev:8443/api":   "https://logbook.boogles.dev:8443/api",
		"https://api.logbook.boogles.dev/api/v1": "https://api.logbook.boogles.dev/api/v1",
		"https://hackatime.hackclub.com/api/v1":  "https://hackatime.hackclub.com/api/v1",
		"":                                       "",
	} {
		if got := MigrateRetired(in); got != want {
			t.Errorf("MigrateRetired(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadDropsRetiredMirror: a mirror aimed at the retired origin was
// Logbook itself, so it is dropped rather than retried forever.
func TestLoadDropsRetiredMirror(t *testing.T) {
	dir := t.TempDir()
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	c.Hackatime = Hackatime{URL: "https://logbook.boogles.dev/api/v1", APIKey: "k", Mirror: true}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hackatime != (Hackatime{}) {
		t.Fatalf("mirror = %+v, want none", got.Hackatime)
	}
}
