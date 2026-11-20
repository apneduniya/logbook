package selfinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarget(t *testing.T) {
	tests := []struct {
		goos, home, lad, want string
		wantErr               bool
	}{
		{"windows", `C:\Users\Ada`, `C:\Users\Ada\AppData\Local`, `C:\Users\Ada\AppData\Local\Programs\Logbook\logbook.exe`, false},
		{"windows", `C:\Users\Ada\`, "", `C:\Users\Ada\AppData\Local\Programs\Logbook\logbook.exe`, false},
		{"windows", "", `D:\Local`, `D:\Local\Programs\Logbook\logbook.exe`, false},
		{"darwin", "/Users/ada", "", "/Users/ada/Library/Application Support/Logbook/bin/logbook", false},
		{"linux", "/home/ada/", "", "/home/ada/.local/bin/logbook", false},
		{"freebsd", "/home/ada", "", "/home/ada/.local/bin/logbook", false},
		{"linux", "", "", "", true},
		{"windows", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.goos+tt.want, func(t *testing.T) {
			got, err := Target(tt.goos, tt.home, tt.lad)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("Target() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInstallFreshUpgradeAndSame(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "download", "logbook")
	dst := filepath.Join(root, "Logbook", "bin", "logbook")
	write(t, src, "v1")
	i := New(dst)

	r, err := i.Install(src)
	if err != nil || !r.Changed {
		t.Fatalf("Install() = %+v, %v", r, err)
	}
	if err := i.Finish(r); err != nil {
		t.Fatal(err)
	}
	if content(t, dst) != "v1" {
		t.Fatal("not copied")
	}
	if fi, err := os.Stat(dst); err != nil || (fi.Mode().Perm()&0o100 == 0 && os.PathSeparator == '/') {
		t.Fatalf("not executable: %v", err)
	}

	// Same bytes: nothing to do.
	if r, err := i.Install(src); err != nil || r.Changed {
		t.Fatalf("Install(same) = %+v, %v", r, err)
	}
	// The installed binary installing itself: nothing to do.
	if r, err := i.Install(dst); err != nil || r.Changed {
		t.Fatalf("Install(self) = %+v, %v", r, err)
	}

	write(t, src, "v2")
	r, err = i.Install(src)
	if err != nil || !r.Changed {
		t.Fatalf("upgrade = %+v, %v", r, err)
	}
	if err := i.Finish(r); err != nil {
		t.Fatal(err)
	}
	if content(t, dst) != "v2" {
		t.Fatal("not upgraded")
	}
	if got := leftovers(t, filepath.Dir(dst)); len(got) != 1 {
		t.Fatalf("leftover files: %q", got)
	}

	removed, err := i.Uninstall()
	if err != nil || !removed {
		t.Fatalf("Uninstall() = %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Logbook")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own directories not removed")
	}
	if removed, err := i.Uninstall(); err != nil || removed {
		t.Fatalf("second Uninstall() = %v, %v", removed, err)
	}
}

func TestRollback(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "bin", "logbook")

	// Fresh install rolled back: the binary is gone.
	write(t, src, "new")
	i := New(dst)
	r, err := i.Install(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Rollback(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new binary not removed")
	}

	// Upgrade rolled back: the old binary is back.
	write(t, dst, "old")
	r, err = i.Install(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Rollback(r); err != nil {
		t.Fatal(err)
	}
	if content(t, dst) != "old" {
		t.Fatal("old binary not restored")
	}
	if got := leftovers(t, filepath.Dir(dst)); len(got) != 1 {
		t.Fatalf("leftover files: %q", got)
	}

	// Nothing changed: nothing to roll back.
	if err := i.Rollback(Result{}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanOldRemovesStaleCopies(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "logbook")
	write(t, dst+".old-abc", "stale")
	write(t, filepath.Join(root, "src"), "x")
	if _, err := New(dst).Install(filepath.Join(root, "src")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + ".old-abc"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale copy kept")
	}
}

func TestDeleteCommand(t *testing.T) {
	got := deleteCommand(`C:\A B\Logbook\logbook.exe.old-x`, `C:\A B\Logbook`)
	for _, want := range []string{
		`del /f /q "C:\A B\Logbook\logbook.exe.old-x"`,
		`if exist "C:\A B\Logbook\logbook.exe.old-x" ping -n 2`,
		`rmdir "C:\A B\Logbook"`,
		`for /l %i in (1,1,30)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deleteCommand() lacks %q:\n%s", want, got)
		}
	}
}
