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
