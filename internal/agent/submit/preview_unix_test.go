//go:build unix

package submit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The preview reveals evidence, so it is readable by its owner only, even
// when it replaces a file other local users could read.
func TestPreviewIsPrivate(t *testing.T) {
	f := newFixture(t)
	for name, existing := range map[string]bool{"a new file": false, "over a 0644 file": true} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "preview.json")
			if existing {
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil { // as a umask of 022 would leave it
					t.Fatal(err)
				}
			}
			req := request()
			req.Preview = path
			var out bytes.Buffer
			if err := Run(context.Background(), req, env(f, &fakeServer{}, "", false, &out)); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := st.Mode().Perm(); perm != 0o600 {
				t.Fatalf("preview mode = %o, want 600", perm)
			}
			if st.Size() <= int64(len("old")) {
				t.Fatalf("preview not written: %d bytes", st.Size())
			}
			left, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*"))
			if err != nil || len(left) != 0 {
				t.Fatalf("temporary files left: %v, %v", left, err)
			}
		})
	}
}
