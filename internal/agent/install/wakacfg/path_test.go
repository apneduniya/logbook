package wakacfg

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLocate(t *testing.T) {
	// Absolute on every OS ("/xdg" is not absolute on Windows). Nothing is
	// created there: Exists is faked.
	base := t.TempDir()
	home := filepath.Join(base, "home")
	wakaHome := filepath.Join(base, "w")
	xdg := filepath.Join(base, "xdg")
	legacy := filepath.Join(home, ".wakatime.cfg")
	xdgDefault := filepath.Join(home, ".config", "wakatime", "wakatime.cfg")
	xdgCustom := filepath.Join(xdg, "wakatime", "wakatime.cfg")
	tests := []struct {
		name   string
		env    map[string]string
		exists []string
		want   string
	}{
		{"WAKATIME_HOME wins", map[string]string{"WAKATIME_HOME": wakaHome}, []string{legacy}, filepath.Join(wakaHome, ".wakatime.cfg")},
		{"WAKATIME_HOME with tilde", map[string]string{"WAKATIME_HOME": "~/waka"}, nil, filepath.Join(home, "waka", ".wakatime.cfg")},
		{"bad tilde falls back to home", map[string]string{"WAKATIME_HOME": "~other/x"}, nil, legacy},
		{"legacy when it exists", nil, []string{legacy, xdgDefault}, legacy},
		{"xdg default when only it exists", nil, []string{xdgDefault}, xdgDefault},
		{"xdg custom", map[string]string{"XDG_CONFIG_HOME": xdg}, []string{xdgCustom}, xdgCustom},
		{"relative xdg ignored", map[string]string{"XDG_CONFIG_HOME": "rel"}, []string{xdgDefault}, xdgDefault},
		{"nothing exists: create legacy", nil, nil, legacy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := Env{
				Getenv:  func(k string) string { return tt.env[k] },
				HomeDir: func() (string, error) { return home, nil },
				Exists: func(p string) bool {
					for _, e := range tt.exists {
						if e == p {
							return true
						}
					}
					return false
				},
			}
			got, err := Locate(env)
			if err != nil || got != tt.want {
				t.Fatalf("Locate() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestLocateNoHome(t *testing.T) {
	env := Env{
		Getenv:  func(string) string { return "" },
		HomeDir: func() (string, error) { return "", errors.New("no home") },
		Exists:  func(string) bool { return false },
	}
	if _, err := Locate(env); err == nil {
		t.Fatal("Locate() = nil error")
	}
}
