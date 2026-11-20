package selfinstall

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// maxBinarySize bounds the copy; the agent is about 15 MB.
const maxBinarySize = 256 << 20

// oldSuffix marks a previous binary renamed aside.
const oldSuffix = ".old"

// Installer copies binaries to one destination path.
type Installer struct {
	dst string
	// deleteLater removes a file that may still be running. On Windows it
	// asks a hidden helper to delete it once the process exits; elsewhere
	// it deletes it at once.
	deleteLater func(path string) error
}

// New returns an Installer for dst (see Target).
func New(dst string) *Installer {
	return &Installer{dst: dst, deleteLater: deleteLater}
}

// Path returns the destination.
func (i *Installer) Path() string { return i.dst }

// Result says what Install did. Pass it to Rollback or Finish.
type Result struct {
	Path string
	// Changed means a new binary was written.
	Changed bool
	// previous is the old binary, renamed aside, or "" if there was none.
	previous string
}

// Install copies src to the destination. Copying the installed binary onto
// itself, or a binary identical to the installed one, changes nothing.
// An existing binary is renamed aside (it may be running) and kept until
// Finish or Rollback.
func (i *Installer) Install(src string) (Result, error) {
	res := Result{Path: i.dst}
	i.cleanOld()
	same, err := sameContent(src, i.dst)
	if err != nil {
		return res, err
	}
	if same {
		return res, nil
	}
	dir := filepath.Dir(i.dst)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a bin directory others may need to execute from
		return res, fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := copyToTemp(src, dir)
	if err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed into place

	if _, err := os.Lstat(i.dst); err == nil {
		res.previous = i.dst + oldSuffix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := os.Rename(i.dst, res.previous); err != nil {
			return res, fmt.Errorf("moving the old binary aside: %w", err)
		}
	}
	if err := os.Rename(tmp, i.dst); err != nil {
		if res.previous != "" {
			_ = os.Rename(res.previous, i.dst) // best effort: put the old one back; the copy error is what matters
		}
		return res, fmt.Errorf("installing binary: %w", err)
	}
	res.Changed = true
	return res, nil
}

// Finish deletes the previous binary once the install has succeeded.
func (i *Installer) Finish(r Result) error {
	if r.previous == "" {
		return nil
	}
	return i.deleteLater(r.previous)
}

// Rollback undoes Install: it puts the previous binary back, or removes the
// new one if there was none.
func (i *Installer) Rollback(r Result) error {
	if !r.Changed {
		return nil
	}
	if r.previous == "" {
		return i.remove()
	}
	// The new binary may be running (Start ran); rename it aside first.
	aside := i.dst + oldSuffix + "-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(i.dst, aside); err != nil {
		return fmt.Errorf("moving the new binary aside: %w", err)
	}
	if err := os.Rename(r.previous, i.dst); err != nil {
		return fmt.Errorf("restoring the previous binary: %w", err)
	}
	return i.deleteLater(aside)
}

// Uninstall removes the installed binary, including when it is the one
// running. It reports whether there was one. The directory is removed if
// it is left empty and is ours (Windows and macOS); ~/.local/bin is shared
// and stays.
func (i *Installer) Uninstall() (bool, error) {
	if _, err := os.Lstat(i.dst); errors.Is(err, fs.ErrNotExist) {
		i.cleanOld()
		return false, nil
	}
	if err := i.remove(); err != nil {
		return true, err
	}
	i.cleanOld()
	removeOwnDirs(filepath.Dir(i.dst))
	return true, nil
}

// removeOwnDirs removes ".../Logbook/bin" and ".../Logbook" when empty.
// Other directories (~/.local/bin) are shared and never removed.
func removeOwnDirs(dir string) {
	if filepath.Base(dir) == "bin" && filepath.Base(filepath.Dir(dir)) == "Logbook" {
		_ = os.Remove(dir) // fails harmlessly when not empty
		dir = filepath.Dir(dir)
	}
	if filepath.Base(dir) == "Logbook" {
		_ = os.Remove(dir) // fails harmlessly when not empty (or, on Windows, until the helper runs)
	}
}

func (i *Installer) remove() error {
	aside := i.dst + oldSuffix + "-uninstall-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(i.dst, aside); err != nil {
		return fmt.Errorf("removing %s: %w", i.dst, err)
	}
	return i.deleteLater(aside)
}

// cleanOld deletes binaries renamed aside by earlier runs. Ones still
// running (Windows) can't be deleted yet and are left for next time.
func (i *Installer) cleanOld() {
	matches, err := filepath.Glob(i.dst + oldSuffix + "-*")
	if err != nil {
		return // the pattern is fixed and valid; Glob only fails on bad patterns
	}
	for _, m := range matches {
		_ = os.Remove(m) // in use on Windows: retried on the next install
	}
}

// sameContent reports whether a and b are the same file or have the same
// bytes. A missing b is not the same.
func sameContent(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", a, err)
	}
	fb, err := os.Stat(b)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", b, err)
	}
	if os.SameFile(fa, fb) {
		return true, nil
	}
	if fa.Size() != fb.Size() {
		return false, nil
	}
	ha, err := hashFile(a)
	if err != nil {
		return false, err
	}
	hb, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ha, hb), nil
}

func hashFile(p string) ([]byte, error) {
	f, err := os.Open(p) //nolint:gosec // our own binary paths
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p, err)
	}
	defer func() { _ = f.Close() }() // read-only
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxBinarySize)); err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	return h.Sum(nil), nil
}

// copyToTemp copies src into a new executable temp file in dir.
func copyToTemp(src, dir string) (string, error) {
	in, err := os.Open(src) //nolint:gosec // src is this program's own executable
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", src, err)
	}
	defer func() { _ = in.Close() }() // read-only
	out, err := os.CreateTemp(dir, ".logbook-new-*")
	if err != nil {
		return "", fmt.Errorf("creating temp binary: %w", err)
	}
	fail := func(err error) (string, error) {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return "", err
	}
	n, err := io.Copy(out, io.LimitReader(in, maxBinarySize+1))
	if err != nil {
		return fail(fmt.Errorf("copying binary: %w", err))
	}
	if n > maxBinarySize {
		return fail(fmt.Errorf("%s is larger than %d bytes", src, maxBinarySize))
	}
	if err := out.Chmod(0o755); err != nil { //nolint:gosec // an executable
		return fail(fmt.Errorf("chmod binary: %w", err))
	}
	if err := out.Sync(); err != nil {
		return fail(fmt.Errorf("syncing binary: %w", err))
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(out.Name())
		return "", fmt.Errorf("closing binary: %w", err)
	}
	return out.Name(), nil
}
