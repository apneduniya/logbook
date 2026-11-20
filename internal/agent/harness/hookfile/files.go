package hookfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
)

// maxConfigSize bounds how much of a harness config file is read. Real ones
// are a few kilobytes; anything this big isn't a config file we should edit.
const maxConfigSize = 8 << 20

// ErrTooLarge means the config file is bigger than maxConfigSize.
var ErrTooLarge = errors.New("config file is too large to edit safely")

// Resolve follows symlinks so writes land on the real file (dotfile managers
// often symlink harness settings into a repo). A missing file resolves to
// itself.
func Resolve(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// The link target (or the file) doesn't exist yet. If path itself is a
		// dangling symlink, write through it to its target.
		if link, lerr := os.Readlink(path); lerr == nil {
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(path), link)
			}
			return link, nil
		}
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", path, err)
	}
	return target, nil
}

// ReadFile reads a config file of at most 8 MiB, reporting whether it
// existed and its permission bits.
func ReadFile(path string) (data []byte, existed bool, mode fs.FileMode, err error) {
	f, err := os.Open(path) //nolint:gosec // path is a harness config file under the user's home
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, 0, nil
	}
	if err != nil {
		return nil, false, 0, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error can't lose data
	st, err := f.Stat()
	if err != nil {
		return nil, false, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, false, 0, fmt.Errorf("%s is not a regular file", path)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, false, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxConfigSize {
		return nil, false, 0, fmt.Errorf("%s: %w", path, ErrTooLarge)
	}
	return data, true, st.Mode().Perm(), nil
}

// WriteAtomic replaces path with data via a temp file and rename, so a crash
// never leaves a half-written config. It creates the parent directory if
// needed.
func WriteAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the harness's own config dir under home
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".logbook-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	// On Windows the rename can fail briefly while an editor or the harness
	// holds the file open (Cursor watches hooks.json), so retry a few times.
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmp.Name(), path)
		if err == nil || attempt == 4 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// backupNames returns the names of the copy of the original file and the
// marker saying there was no original, inside the backup dir.
func backupNames(dir, harness, path string) (orig, absent string) {
	base := harness + "." + filepath.Base(path)
	return filepath.Join(dir, base+".orig"), filepath.Join(dir, base+".absent")
}

// BackupOnce saves the state of path before Logbook's first change to it: a
// copy of the file if it existed, or an empty marker if it didn't. Later
// installs keep that first backup.
func BackupOnce(dir, harness, path string, original []byte, existed bool) error {
	orig, absent := backupNames(dir, harness, path)
	if fileExists(orig) || fileExists(absent) {
		return nil
	}
	target, content := absent, []byte(nil)
	if existed {
		target, content = orig, original
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		return fmt.Errorf("backing up %s: %w", path, err)
	}
	return nil
}

// originalKeys reads the backup of path and returns the original file's
// top-level keys, or nil if Logbook created the file. known is false when
// there is no usable backup.
func originalKeys(dir, harness, path string, jsonc bool) (keys map[string]bool, known bool) {
	orig, absent := backupNames(dir, harness, path)
	if fileExists(absent) {
		return nil, true
	}
	data, existed, _, err := ReadFile(orig)
	if err != nil || !existed {
		return nil, false
	}
	if jsonc {
		data, _ = jsonedit.StripJSONC(data)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	keys = map[string]bool{}
	if len(bytes.TrimSpace(data)) == 0 {
		return keys, true
	}
	doc, err := jsonedit.ParseObject(data)
	if err != nil {
		return nil, false
	}
	for _, k := range doc.Keys() {
		keys[k] = true
	}
	return keys, true
}

// forgetBackup removes the backup records for path once Logbook has deleted
// the file it created, so a later install starts fresh.
func forgetBackup(dir, harness, path string) error {
	_, absent := backupNames(dir, harness, path)
	if err := os.Remove(absent); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing backup marker: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
