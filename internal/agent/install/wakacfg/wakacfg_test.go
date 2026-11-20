package wakacfg

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	localURL   = "http://127.0.0.1:47231/api/v1"
	logbookKey = "00000000-0000-4000-8000-000000000000"
)

// setup copies fixture (or nothing, for "") to a fresh home and returns a
// Manager for it.
func setup(t *testing.T, fixture string) (*Manager, string, []byte) {
	t.Helper()
	home := t.TempDir()
	data := t.TempDir()
	cfg := filepath.Join(home, ".wakatime.cfg")
	var orig []byte
	if fixture != "" {
		b, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		orig = b
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg, data, ""), cfg, orig
}

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInstallUninstallFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		golden  string
		want    Original
	}{
		{"", "missing.golden", Original{}},
		{"hackatime-setup.cfg", "hackatime-setup.golden", Original{
			URL: "https://hackatime.hackclub.com/api/hackatime/v1", HadURL: true,
			APIKey: "11111111-1111-4111-8111-111111111111", HadKey: true}},
		{"hackatime-docs.cfg", "hackatime-docs.golden", Original{
			URL: "https://hackatime.hackclub.com/api/hackatime/v1", HadURL: true,
			APIKey: "11111111-1111-4111-8111-111111111111", HadKey: true}},
		{"wakatime-crlf-bom.cfg", "wakatime-crlf-bom.golden", Original{
			APIKey: "waka_22222222-2222-4222-8222-222222222222", HadKey: true}},
		{"wakatime-sections.cfg", "wakatime-sections.golden", Original{
			APIKey: "waka_33333333-3333-4333-8333-333333333333", HadKey: true}},
		{"no-settings.cfg", "no-settings.golden", Original{}},
		// Only the legacy "apikey" spelling, which wakatime-cli still reads
		// when api_key is empty: it is the key to mirror with, but it is
		// not an api_key to write back on uninstall.
		{"wakatime-legacy-apikey.cfg", "wakatime-legacy-apikey.golden", Original{
			APIKey: "waka_44444444-4444-4444-8444-444444444444"}},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			m, cfg, orig := setup(t, tt.fixture)

			res, err := m.Install(localURL, logbookKey)
			if err != nil {
				t.Fatalf("Install() = %v", err)
			}
			if !res.Changed || res.AlreadyInstalled {
				t.Fatalf("first Install() = %+v, want Changed and not AlreadyInstalled", res)
			}
			if res.Original != tt.want {
				t.Fatalf("Original = %+v, want %+v", res.Original, tt.want)
			}
			if res.BackupWritten != (tt.fixture != "") {
				t.Fatalf("BackupWritten = %v", res.BackupWritten)
			}
			got := read(t, cfg)
			golden := filepath.Join("testdata", tt.golden)
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if want := read(t, golden); !bytes.Equal(got, want) {
				t.Fatalf("installed file:\n%q\nwant:\n%q", got, want)
			}
			if tt.fixture != "" && !bytes.Equal(read(t, m.BackupPath()), orig) {
				t.Fatal("backup differs from the original")
			}
			if ok, err := m.Installed(); err != nil || !ok {
				t.Fatalf("Installed() = %v, %v", ok, err)
			}

			// Idempotent: a second install changes nothing.
			res2, err := m.Install(localURL, logbookKey)
			if err != nil {
				t.Fatalf("second Install() = %v", err)
			}
			if res2.Changed || !res2.AlreadyInstalled || res2.BackupWritten || res2.Original != tt.want {
				t.Fatalf("second Install() = %+v", res2)
			}
			if !bytes.Equal(read(t, cfg), got) {
				t.Fatal("second Install() changed the file")
			}

			un, err := m.Uninstall()
			if err != nil {
				t.Fatalf("Uninstall() = %v", err)
			}
			if tt.fixture == "" {
				if un.Restored != "removed" {
					t.Fatalf("Restored = %q, want removed", un.Restored)
				}
				if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("config still exists: %v", err)
				}
			} else {
				if un.Restored != "exact" {
					t.Fatalf("Restored = %q, want exact", un.Restored)
				}
				if !bytes.Equal(read(t, cfg), orig) {
					t.Fatalf("restored file differs:\n%q\nwant\n%q", read(t, cfg), orig)
				}
			}
			for _, p := range []string{m.BackupPath(), m.statePath()} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left behind", p)
				}
			}
			un2, err := m.Uninstall()
			if err != nil || !un2.NotInstalled {
				t.Fatalf("second Uninstall() = %+v, %v", un2, err)
			}
		})
	}
}

func TestUninstallAfterUserEditRestoresOnlyKeys(t *testing.T) {
	m, cfg, _ := setup(t, "hackatime-docs.cfg")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(read(t, cfg)), "heartbeat_rate_limit_seconds = 30", "heartbeat_rate_limit_seconds = 60", 1) +
		"\n[projectmap]\nfoo = bar\n"
	if err := os.WriteFile(cfg, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	un, err := m.Uninstall()
	if err != nil {
		t.Fatal(err)
	}
	if un.Restored != "keys" || un.BackupKept != m.BackupPath() {
		t.Fatalf("Uninstall() = %+v", un)
	}
	got := string(read(t, cfg))
	for _, want := range []string{
		"api_url = https://hackatime.hackclub.com/api/hackatime/v1",
		"api_key = 11111111-1111-4111-8111-111111111111",
		"heartbeat_rate_limit_seconds = 60",
		"[projectmap]\nfoo = bar",
		"# put this in your ~/.wakatime.cfg file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("restored file lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, localURL) || strings.Contains(got, logbookKey) {
		t.Errorf("restored file still has Logbook values:\n%s", got)
	}
}

func TestUninstallAfterUserEditOfCreatedFileRemovesKeys(t *testing.T) {
	m, cfg, _ := setup(t, "")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(read(t, cfg), "debug = true\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	un, err := m.Uninstall()
	if err != nil || un.Restored != "keys" {
		t.Fatalf("Uninstall() = %+v, %v", un, err)
	}
	if got := string(read(t, cfg)); got != "[settings]\ndebug = true\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestUninstallLeavesRepointedFileAlone(t *testing.T) {
	m, cfg, _ := setup(t, "hackatime-setup.cfg")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	mine := "[settings]\napi_url = https://example.com/api/v1\napi_key = mine\n"
	if err := os.WriteFile(cfg, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Installed(); err != nil || ok {
		t.Fatalf("Installed() = %v, %v; want false", ok, err)
	}
	un, err := m.Uninstall()
	if err != nil || un.Restored != "" || un.Note == "" {
		t.Fatalf("Uninstall() = %+v, %v", un, err)
	}
	if string(read(t, cfg)) != mine {
		t.Fatal("Uninstall() changed a file that no longer points at Logbook")
	}
}

func TestExistingBackupIsNeverOverwritten(t *testing.T) {
	m, cfg, _ := setup(t, "hackatime-setup.cfg")
	old := []byte("[settings]\napi_key = old\n")
	if err := os.WriteFile(m.BackupPath(), old, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := m.Install(localURL, logbookKey)
	if err != nil || res.BackupWritten {
		t.Fatalf("Install() = %+v, %v", res, err)
	}
	if !bytes.Equal(read(t, m.BackupPath()), old) {
		t.Fatal("backup was overwritten")
	}
	// The stale backup can't be restored verbatim, so only keys go back.
	un, err := m.Uninstall()
	if err != nil || un.Restored != "keys" || un.BackupKept == "" {
		t.Fatalf("Uninstall() = %+v, %v", un, err)
	}
	if !strings.Contains(string(read(t, cfg)), "api_key = 11111111-1111-4111-8111-111111111111") {
		t.Fatalf("key not restored:\n%s", read(t, cfg))
	}
}

func TestReinstallUpdatesKeyButKeepsOriginal(t *testing.T) {
	m, cfg, _ := setup(t, "hackatime-setup.cfg")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	const newKey = "99999999-9999-4999-8999-999999999999"
	res, err := m.Install(localURL, newKey)
	if err != nil || !res.Changed || !res.AlreadyInstalled {
		t.Fatalf("Install() = %+v, %v", res, err)
	}
	if res.Original.APIKey != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("Original lost on reinstall: %+v", res.Original)
	}
	if !strings.Contains(string(read(t, cfg)), "api_key = "+newKey) {
		t.Fatal("new key not written")
	}
	// The file changed after the backup, but only by us: still exact.
	un, err := m.Uninstall()
	if err != nil || un.Restored != "exact" {
		t.Fatalf("Uninstall() = %+v, %v", un, err)
	}
}

func TestImportCfgOverridingKeysFails(t *testing.T) {
	m, cfg, _ := setup(t, "")
	imp := filepath.Join(t.TempDir(), "secret.cfg")
	if err := os.WriteFile(imp, []byte("[settings]\napi_key = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := "[settings]\nimport_cfg = " + imp + "\n"
	if err := os.WriteFile(cfg, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(localURL, logbookKey); !errors.Is(err, ErrImportOverrides) {
		t.Fatalf("Install() = %v, want ErrImportOverrides", err)
	}
	if string(read(t, cfg)) != orig {
		t.Fatal("file changed despite the error")
	}
}

func TestEnvKeyIsCapturedButNotWrittenBack(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	cfg := filepath.Join(home, ".wakatime.cfg")
	if err := os.WriteFile(cfg, []byte("[settings]\ndebug = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(cfg, data, "envkey")
	res, err := m.Install(localURL, logbookKey)
	if err != nil {
		t.Fatal(err)
	}
	if res.Original.APIKey != "envkey" || res.Original.HadKey {
		t.Fatalf("Original = %+v", res.Original)
	}
}

func TestOtherFileStateRefuses(t *testing.T) {
	m, _, _ := setup(t, "hackatime-setup.cfg")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	other := New(filepath.Join(t.TempDir(), ".wakatime.cfg"), m.dataDir, "")
	if _, err := other.Install(localURL, logbookKey); !errors.Is(err, ErrOtherFile) {
		t.Fatalf("Install() = %v, want ErrOtherFile", err)
	}
}

func TestSymlinkAndModeArePreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and Unix modes")
	}
	home, data := t.TempDir(), t.TempDir()
	linkTarget := filepath.Join(t.TempDir(), "dotfiles.cfg")
	if err := os.WriteFile(linkTarget, []byte("[settings]\napi_key = k\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".wakatime.cfg")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatal(err)
	}
	m := New(link, data, "")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi.Mode(), err)
	}
	st, err := os.Stat(linkTarget)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, %v; want 0640", st.Mode().Perm(), err)
	}
	if !strings.Contains(string(read(t, linkTarget)), localURL) {
		t.Fatal("link target not updated")
	}
	if _, err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if string(read(t, linkTarget)) != "[settings]\napi_key = k\n" {
		t.Fatalf("not restored: %q", read(t, linkTarget))
	}
}

func TestWarnings(t *testing.T) {
	d := parseDoc([]byte("[settings]\napi_key_vault_cmd = pass waka\n[api_urls]\n^/work/ = https://x/api/v1|k\n^/b/ = y\n[project_api_key]\nfoo = k\n"))
	if got := warnings(d); len(got) != 3 {
		t.Fatalf("warnings() = %q, want 3", got)
	}
}

func TestReinstallAfterUserEditKeepsTheEdit(t *testing.T) {
	m, cfg, _ := setup(t, "hackatime-setup.cfg")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	// The user (or a plugin) edits the file, then runs install again, as
	// the docs say to do when upgrading.
	if err := os.WriteFile(cfg, append(read(t, cfg), "\n[git]\nsubmodules_disabled = true\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := m.Install(localURL, logbookKey); err != nil || !res.AlreadyInstalled {
		t.Fatalf("Install() = %+v, %v", res, err)
	}
	un, err := m.Uninstall()
	if err != nil || un.Restored != "keys" {
		t.Fatalf("Uninstall() = %+v, %v; want a keys-only restore", un, err)
	}
	got := string(read(t, cfg))
	if !strings.Contains(got, "[git]\nsubmodules_disabled = true") {
		t.Fatalf("the edit made between installs was lost:\n%s", got)
	}
	if !strings.Contains(got, "api_key = 11111111-1111-4111-8111-111111111111") || strings.Contains(got, localURL) {
		t.Fatalf("keys not restored:\n%s", got)
	}
}

func TestLegacyAPIKeyUninstallAfterEdit(t *testing.T) {
	m, cfg, _ := setup(t, "wakatime-legacy-apikey.cfg")
	if _, err := m.Install(localURL, logbookKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(read(t, cfg), "[projectmap]\nx = y\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if un, err := m.Uninstall(); err != nil || un.Restored != "keys" {
		t.Fatalf("Uninstall() = %+v, %v", un, err)
	}
	got := string(read(t, cfg))
	if strings.Contains(got, "api_key") || !strings.Contains(got, "apikey = waka_44444444-4444-4444-8444-444444444444") {
		t.Fatalf("file = %s", got)
	}
}

func TestRevert(t *testing.T) {
	t.Run("first install", func(t *testing.T) {
		m, cfg, orig := setup(t, "hackatime-setup.cfg")
		res, err := m.Install(localURL, logbookKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Revert(res); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read(t, cfg), orig) {
			t.Fatal("file not restored")
		}
		if _, ok, err := m.loadState(); err != nil || ok {
			t.Fatalf("state left behind: %v, %v", ok, err)
		}
	})
	t.Run("repeat install with a new key", func(t *testing.T) {
		m, cfg, orig := setup(t, "hackatime-setup.cfg")
		if _, err := m.Install(localURL, logbookKey); err != nil {
			t.Fatal(err)
		}
		before := read(t, cfg)
		stBefore := read(t, m.statePath())
		res, err := m.Install(localURL, "99999999-9999-4999-8999-999999999999")
		if err != nil || !res.Changed {
			t.Fatalf("Install() = %+v, %v", res, err)
		}
		if err := m.Revert(res); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read(t, cfg), before) || !bytes.Equal(read(t, m.statePath()), stBefore) {
			t.Fatal("repeat install not reverted to the earlier install")
		}
		// The earlier install is intact: uninstall still restores exactly.
		if un, err := m.Uninstall(); err != nil || un.Restored != "exact" || !bytes.Equal(read(t, cfg), orig) {
			t.Fatalf("Uninstall() = %+v, %v", un, err)
		}
	})
}
