package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LOGBOOK_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("LOGBOOK_WEB_URL", "https://logbook.boogles.dev")
}

func TestLoadDevLogin(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantErr  string
		wantDev  bool
		checkHCA bool
	}{
		{
			name: "dev login on 127.0.0.1",
			env: map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "http://127.0.0.1:8080",
				"LOGBOOK_DEV_INSECURE_COOKIE": "true"},
			wantDev: true,
		},
		{
			name: "dev login on localhost",
			env: map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "http://localhost:5173",
				"LOGBOOK_DEV_INSECURE_COOKIE": "true"},
			wantDev: true,
		},
		{
			name:    "dev login on a public host",
			env:     map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "https://logbook.hackclub.com"},
			wantErr: "LOGBOOK_DEV_LOGIN",
		},
		{
			name:    "dev login on a lookalike host",
			env:     map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "https://localhost.evil.example"},
			wantErr: "LOGBOOK_DEV_LOGIN",
		},
		{
			name: "dev login on ipv6 loopback listener",
			env: map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "http://localhost:8080",
				"LOGBOOK_DEV_INSECURE_COOKIE": "true", "LOGBOOK_ADDR": "[::1]:8080"},
			wantDev: true,
		},
		{
			name: "dev login listening on all interfaces",
			env: map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "http://localhost:8080",
				"LOGBOOK_DEV_INSECURE_COOKIE": "true", "LOGBOOK_ADDR": "0.0.0.0:8080"},
			wantErr: "LOGBOOK_ADDR",
		},
		{
			name: "dev login listening on an empty host",
			env: map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "http://localhost:8080",
				"LOGBOOK_DEV_INSECURE_COOKIE": "true", "LOGBOOK_ADDR": ":8080"},
			wantErr: "LOGBOOK_ADDR",
		},
		{
			name: "dev login listening on a LAN address",
			env: map[string]string{"LOGBOOK_DEV_LOGIN": "true", "LOGBOOK_BASE_URL": "http://localhost:8080",
				"LOGBOOK_DEV_INSECURE_COOKIE": "true", "LOGBOOK_ADDR": "192.168.1.20:8080"},
			wantErr: "LOGBOOK_ADDR",
		},
		{
			name:     "non-loopback listener without dev login",
			env:      map[string]string{"LOGBOOK_BASE_URL": "https://logbook.hackclub.com", "LOGBOOK_ADDR": "0.0.0.0:8080"},
			checkHCA: true,
		},
		{
			name:     "production defaults",
			env:      map[string]string{"LOGBOOK_BASE_URL": "https://logbook.hackclub.com"},
			checkHCA: true,
		},
		{
			name:    "plain-http HCA",
			env:     map[string]string{"LOGBOOK_BASE_URL": "https://logbook.hackclub.com", "LOGBOOK_HCA_BASE_URL": "http://auth.hackclub.com"},
			wantErr: "LOGBOOK_HCA_BASE_URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() err = %v, want mention of %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() err = %v", err)
			}
			if c.DevLogin != tc.wantDev {
				t.Errorf("DevLogin = %v, want %v", c.DevLogin, tc.wantDev)
			}
			if tc.checkHCA && c.HCA.BaseURL != DefaultHCABaseURL {
				t.Errorf("HCA.BaseURL = %q, want default", c.HCA.BaseURL)
			}
		})
	}
}

func TestLoadWebURL(t *testing.T) {
	tests := []struct {
		name       string
		web        string
		devCookie  bool
		wantErr    bool
		wantOrigin string
	}{
		{name: "https origin", web: "https://logbook.boogles.dev", wantOrigin: "https://logbook.boogles.dev"},
		{name: "trailing slash", web: "https://logbook.boogles.dev/", wantOrigin: "https://logbook.boogles.dev"},
		{name: "upper case host", web: "https://Logbook.Boogles.dev", wantOrigin: "https://logbook.boogles.dev"},
		{name: "explicit port", web: "https://logbook.boogles.dev:8443", wantOrigin: "https://logbook.boogles.dev:8443"},
		{name: "dev default", web: "", devCookie: true, wantOrigin: DevWebURL},
		{name: "dev http loopback", web: "http://127.0.0.1:5173", devCookie: true, wantOrigin: "http://127.0.0.1:5173"},
		{name: "missing in production", web: "", wantErr: true},
		{name: "http loopback outside dev", web: "http://localhost:5173", wantErr: true},
		{name: "http public host in dev", web: "http://logbook.boogles.dev", devCookie: true, wantErr: true},
		{name: "path", web: "https://logbook.boogles.dev/app", wantErr: true},
		{name: "query", web: "https://logbook.boogles.dev/?x=1", wantErr: true},
		{name: "fragment", web: "https://logbook.boogles.dev/#x", wantErr: true},
		{name: "user info", web: "https://user@logbook.boogles.dev", wantErr: true},
		{name: "relative", web: "logbook.boogles.dev", wantErr: true},
		{name: "other scheme", web: "ftp://logbook.boogles.dev", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("LOGBOOK_WEB_URL", tc.web)
			if tc.devCookie {
				t.Setenv("LOGBOOK_DEV_INSECURE_COOKIE", "true")
			} else {
				t.Setenv("LOGBOOK_BASE_URL", "https://api.logbook.boogles.dev")
			}
			c, err := Load()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "LOGBOOK_WEB_URL") {
					t.Fatalf("Load() err = %v, want a LOGBOOK_WEB_URL error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() err = %v", err)
			}
			if got := c.WebOrigin(); got != tc.wantOrigin {
				t.Errorf("WebOrigin() = %q, want %q", got, tc.wantOrigin)
			}
		})
	}
}
