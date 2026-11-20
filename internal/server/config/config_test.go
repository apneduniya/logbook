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
