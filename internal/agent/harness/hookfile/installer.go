package hookfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
)

// Spec describes one harness's hook config file.
type Spec struct {
	// Harness is the harness id used in hook command lines and backups.
	Harness string
	// Path returns the config file's location.
	Path func() (string, error)
	// Layout is the hooks shape.
	Layout Layout
	// JSONC allows comments and trailing commas in the existing file.
	// Comments are not kept when Logbook rewrites it (the backup has them).
	JSONC bool
	// Header lists top-level keys a new file starts with, in order (for
	// example Cursor's "version": 1). Existing values are never changed.
	Header []Field
	// Entries returns the hooks to install for binPath.
	Entries func(binPath string) []Entry
	// Argv reads the command out of a handler.
	Argv ArgvFunc
	// Detect reports whether the harness looks installed.
	Detect func() bool
	// BackupDir returns where backups go.
	BackupDir func() (string, error)
}

// Field is one top-level key and its raw JSON value.
type Field struct {
	Key   string
	Value json.RawMessage
}

// Installer installs a harness's hooks into its JSON config file.
type Installer struct {
	spec Spec
	ed   editor
}

// New returns an Installer for spec.
func New(spec Spec) *Installer {
	ours := func(h json.RawMessage) bool { return cmdline.IsHook(spec.Argv(h), spec.Harness) }
	return &Installer{spec: spec, ed: editor{layout: spec.Layout, ours: ours}}
}

// Name returns the harness id.
func (in *Installer) Name() string { return in.spec.Harness }

// Detect reports whether the harness looks installed on this machine.
func (in *Installer) Detect() bool { return in.spec.Detect() }

// ConfigPath returns the config file Logbook edits.
func (in *Installer) ConfigPath() (string, error) { return in.spec.Path() }

// ErrRelativeBin means Install was given a relative binary path. Hooks run
// with the harness's working directory, so the path must be absolute.
var ErrRelativeBin = errors.New("logbook binary path must be absolute")

// Install adds or updates Logbook's hooks. Running it again with the same
// binPath changes nothing.
func (in *Installer) Install(binPath string) error {
	if !filepath.IsAbs(binPath) {
		return fmt.Errorf("%s: %w: %q", in.spec.Harness, ErrRelativeBin, binPath)
	}
	path, doc, original, existed, mode, err := in.load()
	if err != nil {
		return err
	}
	changed, err := in.ed.apply(doc, in.spec.Entries(binPath))
	if err != nil {
		return fmt.Errorf("%s: %s: %w", in.spec.Harness, path, err)
	}
	if !changed {
		return nil
	}
	in.addHeader(doc)
	dir, err := in.spec.BackupDir()
	if err != nil {
		return fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	if err := backupOnce(dir, in.spec.Harness, path, original, existed); err != nil {
		return fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	return in.save(path, doc, mode)
}

// Uninstall removes Logbook's hooks and nothing else. Keys Logbook added to
// the file ("hooks" when it ends up empty, and Header keys) are removed too
// when the backup shows the original didn't have them, and a file Logbook
// created is deleted once nothing else is in it.
func (in *Installer) Uninstall() error {
	path, doc, _, existed, mode, err := in.load()
	if err != nil || !existed {
		return err
	}
	changed, err := in.ed.remove(doc)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", in.spec.Harness, path, err)
	}
	if !changed {
		return nil
	}
	dir, err := in.spec.BackupDir()
	if err != nil {
		return fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	orig, known := originalKeys(dir, in.spec.Harness, path, in.spec.JSONC)
	in.tidy(doc, orig, known)
	if known && orig == nil && doc.Len() == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%s: removing %s: %w", in.spec.Harness, path, err)
		}
		return forgetBackup(dir, in.spec.Harness, path)
	}
	return in.save(path, doc, mode)
}

// tidy removes the top-level keys Logbook added. orig holds the original
// file's top-level keys (nil if there was no file); known is false when no
// backup exists, in which case only an empty "hooks" object is removed and
// header keys are kept.
//
// When Logbook created the file, its Header keys go only if nothing else is
// left: a user who later added their own hooks needs the header (Cursor
// rejects hooks.json without "version"). When the file existed, a Header key
// it didn't have is removed so the user's original comes back.
func (in *Installer) tidy(doc *jsonedit.Object, orig map[string]bool, known bool) {
	if raw, ok := doc.Get("hooks"); ok && jsonedit.Equal(raw, json.RawMessage("{}")) && (!known || !orig["hooks"]) {
		doc.Delete("hooks")
	}
	if !known {
		return
	}
	if orig == nil && !in.onlyHeader(doc) {
		return
	}
	for _, f := range in.spec.Header {
		if v, ok := doc.Get(f.Key); ok && !orig[f.Key] && jsonedit.Equal(v, f.Value) {
			doc.Delete(f.Key)
		}
	}
}

// onlyHeader reports whether every top-level key in doc is a Header key.
func (in *Installer) onlyHeader(doc *jsonedit.Object) bool {
	for _, k := range doc.Keys() {
		header := false
		for _, f := range in.spec.Header {
			if f.Key == k {
				header = true
				break
			}
		}
		if !header {
			return false
		}
	}
	return true
}

// Installed reports whether any Logbook hook is in the config file.
func (in *Installer) Installed() (bool, error) {
	_, doc, _, existed, _, err := in.load()
	if err != nil || !existed {
		return false, err
	}
	n, err := in.ed.count(doc)
	if err != nil {
		return false, fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	return n > 0, nil
}

// load reads and parses the config file. A missing or blank file is an empty
// object.
func (in *Installer) load() (path string, doc *jsonedit.Object, original []byte, existed bool, mode fs.FileMode, err error) {
	p, err := in.spec.Path()
	if err != nil {
		return "", nil, nil, false, 0, fmt.Errorf("%s: finding config: %w", in.spec.Harness, err)
	}
	path, err = resolve(p)
	if err != nil {
		return "", nil, nil, false, 0, fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	original, existed, mode, err = ReadFile(path)
	if err != nil {
		return "", nil, nil, false, 0, fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	if !existed {
		mode = 0o600
	}
	body := original
	if in.spec.JSONC {
		body, _ = jsonedit.StripJSONC(body)
	} else {
		body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return path, jsonedit.NewObject(), original, existed, mode, nil
	}
	doc, err = jsonedit.ParseObject(body)
	if err != nil {
		return "", nil, nil, false, 0, fmt.Errorf("%s: %s is not valid JSON, leaving it alone: %w", in.spec.Harness, path, err)
	}
	return path, doc, original, existed, mode, nil
}

func (in *Installer) save(path string, doc *jsonedit.Object, mode fs.FileMode) error {
	out, err := doc.Indented()
	if err != nil {
		return fmt.Errorf("%s: encoding %s: %w", in.spec.Harness, path, err)
	}
	if err := WriteAtomic(path, out, mode); err != nil {
		return fmt.Errorf("%s: %w", in.spec.Harness, err)
	}
	return nil
}

// addHeader puts the Header keys a file is missing at its start.
func (in *Installer) addHeader(doc *jsonedit.Object) {
	var missing []Field
	for _, f := range in.spec.Header {
		if _, ok := doc.Get(f.Key); !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return
	}
	rest := jsonedit.NewObject()
	for _, k := range doc.Keys() {
		v, _ := doc.Get(k)
		rest.Set(k, v)
	}
	for _, k := range doc.Keys() {
		doc.Delete(k)
	}
	for _, f := range missing {
		doc.Set(f.Key, f.Value)
	}
	for _, k := range rest.Keys() {
		v, _ := rest.Get(k)
		doc.Set(k, v)
	}
}
