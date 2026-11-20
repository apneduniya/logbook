package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/account"
	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/upload"
)

const secretKey = "waka_00000000-0000-4000-8000-0000000000ff"

func healthy() Facts {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	return Facts{
		Now:        now,
		Config:     config.Config{ServerURL: "https://lb.example", APIKey: secretKey, LocalAddr: "127.0.0.1:47231"},
		ServerTime: now.Add(3 * time.Second),
		Wakatime:   WakatimeCfg{Path: "~/.wakatime.cfg", APIURL: "http://127.0.0.1:47231/api/v1", KeyMatches: true},
	}
}

func levels(fs []Finding) map[string]Level {
	m := map[string]Level{}
	for _, f := range fs {
		m[f.Check] = f.Level
	}
	return m
}

func TestDiagnose(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Facts)
		check  string
		want   Level
	}{
		{"healthy key", func(*Facts) {}, "api key", OK},
		{"healthy agent", func(*Facts) {}, "agent", OK},
		{"healthy cfg", func(*Facts) {}, "wakatime.cfg", OK},
		{"healthy uploads", func(*Facts) {}, "uploads", OK},
		{"no config", func(f *Facts) { f.ConfigErr = config.ErrNotConfigured }, "config", Fail},
		{"signed out", func(f *Facts) { f.Config.APIKey = "" }, "api key", Fail},
		{"rejected key", func(f *Facts) { f.VerifyErr = account.ErrKeyRejected }, "api key", Fail},
		{"offline", func(f *Facts) { f.VerifyErr = errors.New("dial tcp: timeout") }, "api key", Warn},
		{"clock skew", func(f *Facts) { f.ServerTime = f.Now.Add(-5 * time.Minute) }, "clock", Warn},
		{"daemon down", func(f *Facts) { f.DaemonErr = errors.New("refused") }, "agent", Fail},
		{"cfg missing", func(f *Facts) { f.Wakatime.Err = fs.ErrNotExist }, "wakatime.cfg", Fail},
		{"cfg points at hackatime", func(f *Facts) {
			f.Wakatime.APIURL = "https://hackatime.hackclub.com/api/hackatime/v1"
		}, "wakatime.cfg", Fail},
		{"cfg wrong port", func(f *Facts) { f.Wakatime.APIURL = "http://localhost:9999/api/v1" }, "wakatime.cfg", Fail},
		{"cfg localhost ok", func(f *Facts) { f.Wakatime.APIURL = "http://localhost:47231/api/v1" }, "wakatime.cfg", OK},
		{"cfg key differs", func(f *Facts) { f.Wakatime.KeyMatches = false }, "wakatime.cfg", Fail},
		{"relogin", func(f *Facts) { f.Status.Logbook.Worker.Paused = upload.ReasonReLogin }, "uploads", Fail},
		{"mirror relogin", func(f *Facts) { f.Status.Hackatime.Worker.Paused = upload.ReasonReLogin }, "mirror", Fail},
		{"dropped", func(f *Facts) { f.Status.Logbook.Queue.Dropped = 3 }, "uploads", Warn},
		{"stale queue", func(f *Facts) {
			f.Status.Logbook.Queue = localdb.QueueStats{Pending: 10, Oldest: f.Now.Add(-3 * time.Hour)}
		}, "uploads", Warn},
		{"mirror off", func(f *Facts) { f.Status.Hackatime.Worker.Paused = "mirroring to Hackatime is off" }, "mirror", OK},
		{"spool", func(f *Facts) { f.Status.SpoolBytes = 5 << 20 }, "hook spool", Warn},
		{"db error", func(f *Facts) { f.StatusErr = errors.New("locked") }, "queues", Warn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := healthy()
			tt.mutate(&f)
			got := Diagnose(f)
			if l := levels(got)[tt.check]; l != tt.want {
				t.Fatalf("%s = %q, want %q (%+v)", tt.check, l, tt.want, got)
			}
			for _, fd := range got {
				if fd.Level != OK && fd.Fix == "" {
					t.Errorf("%s has no fix", fd.Check)
				}
				if strings.Contains(fd.Detail+fd.Fix, secretKey) {
					t.Errorf("%s leaks the key", fd.Check)
				}
			}
		})
	}
}

func TestReadWakatimeCfg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".wakatime.cfg")
	content := "# comment\n[settings]\napi_url = http://127.0.0.1:47231/api/v1\napi_key=" + secretKey +
		"\n; other\n[git]\napi_url = nope\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w := ReadWakatimeCfg(path, secretKey)
	if w.Err != nil || w.APIURL != "http://127.0.0.1:47231/api/v1" || !w.KeyMatches {
		t.Fatalf("read = %+v", w)
	}
	if w := ReadWakatimeCfg(path, "other"); w.KeyMatches {
		t.Fatal("different key matched")
	}
	if w := ReadWakatimeCfg(filepath.Join(dir, "missing"), secretKey); !errors.Is(w.Err, fs.ErrNotExist) {
		t.Fatalf("missing err = %v", w.Err)
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://user:pw@host/api?api_key=x"); got != "https://host/api" {
		t.Fatalf("redactURL = %q", got)
	}
}
