package wakacfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/apneduniya/logbook/internal/agent/install/atomicfile"
)

const (
	settings  = "settings"
	keyURL    = "api_url"
	keyAPIKey = "api_key"
	// keyLegacyAPIKey is the older spelling wakatime-cli still reads when
	// api_key is empty (pkg/params apiKeyOrder).
	keyLegacyAPIKey = "apikey"

	// maxConfigSize bounds what we read. Real files are a few hundred bytes.
	maxConfigSize = 1 << 20

	backupName = "wakatime.cfg.backup"
	stateName  = "wakatime.cfg.state.json"
)

// ErrImportOverrides means the file imports another config (import_cfg)
// that sets api_url or api_key. wakatime-cli reads the imported file last,
// so it would override our values and heartbeats would bypass Logbook.
var ErrImportOverrides = errors.New("the import_cfg file sets api_url or api_key, which would override Logbook; remove them from it and retry")

// ErrOtherFile means a previous install edited a different config file
// (for example WAKATIME_HOME changed since).
var ErrOtherFile = errors.New("logbook already manages a different .wakatime.cfg; run `logbook uninstall` first")

// Original is what the config pointed at before Logbook changed it.
type Original struct {
	URL    string `json:"url,omitempty"`
	HadURL bool   `json:"had_url"`
	// APIKey is the original key. It is a secret: it is stored only in
	// the data directory (mode 0700) and never logged or printed.
	APIKey string `json:"api_key,omitempty"`
	HadKey bool   `json:"had_key"`
}

// state is what install remembers so uninstall can undo it exactly.
type state struct {
	Path string `json:"path"`
	// Existed is false when install created the file.
	Existed bool `json:"existed"`
	// RestoreExact means the backup holds the file as it was right before
	// this install, so uninstall may put those bytes back.
	RestoreExact bool `json:"restore_exact"`
	// WrittenSHA256 is the hash of the file as install left it. A
	// different hash at uninstall means the user edited it since.
	WrittenSHA256 string   `json:"written_sha256"`
	LocalURL      string   `json:"local_url"`
	Original      Original `json:"original"`
}

// Manager edits one .wakatime.cfg and keeps its backup and state in the
// agent's data directory.
type Manager struct {
	path    string
	dataDir string
	envKey  string
}

// New returns a Manager for the config file at path (see Locate), keeping
// its backup and state in dataDir. envAPIKey is the WAKATIME_API_KEY
// environment variable, which wakatime-cli uses when the file has no key.
func New(path, dataDir, envAPIKey string) *Manager {
	return &Manager{path: path, dataDir: dataDir, envKey: envAPIKey}
}

// Path returns the config file this Manager edits.
func (m *Manager) Path() string { return m.path }

// BackupPath returns where the original file is copied.
func (m *Manager) BackupPath() string { return filepath.Join(m.dataDir, backupName) }

func (m *Manager) statePath() string { return filepath.Join(m.dataDir, stateName) }

// InstallResult says what Install did.
type InstallResult struct {
	Path string
	// Original is what the file pointed at before Logbook. On a repeat
	// install it comes from the saved state, not the current file.
	Original Original
	// AlreadyInstalled means the file pointed at Logbook before this call.
	AlreadyInstalled bool
	// Changed means the file's bytes changed.
	Changed bool
	// BackupWritten means a backup was made by this call.
	BackupWritten bool
	// Warnings are settings that may route some heartbeats around Logbook.
	Warnings []string

	// prev is what a repeat install replaced, for Revert.
	prev *snapshot
}

// snapshot is the file and state as they were before a repeat install.
type snapshot struct {
	target  string
	raw     []byte
	existed bool
	state   state
}

// Install points api_url at localURL and sets api_key to key. It is
// idempotent: a second call with the same values changes nothing, and the
// original values are captured only on the first install.
func (m *Manager) Install(localURL, key string) (InstallResult, error) {
	res := InstallResult{Path: m.path}
	st, hasState, err := m.loadState()
	if err != nil {
		return res, err
	}
	target, err := m.target()
	if err != nil {
		return res, err
	}
	raw, existed, err := readBounded(target)
	if err != nil {
		return res, err
	}
	d := parseDoc(raw)
	if err := checkImport(d); err != nil {
		return res, err
	}
	res.Warnings = warnings(d)

	if hasState && st.Path != m.path {
		return res, ErrOtherFile
	}
	cur, _ := d.get(settings, keyURL)
	res.AlreadyInstalled = hasState && sameURL(cur, st.LocalURL)

	if res.AlreadyInstalled {
		res.prev = &snapshot{target: target, raw: raw, existed: existed, state: st}
		if digest(raw) != st.WrittenSHA256 {
			// Someone edited the file since Logbook last wrote it. Those
			// edits are not ours, so the backup no longer matches what an
			// exact restore would have to preserve: restore keys only.
			st.RestoreExact = false
		}
		st.LocalURL = localURL
	} else {
		st = state{Path: m.path, Existed: existed, LocalURL: localURL, Original: m.original(d, localURL)}
		st.RestoreExact, res.BackupWritten, err = m.backup(raw, existed)
		if err != nil {
			return res, err
		}
	}
	res.Original = st.Original

	if err := d.set(settings, keyURL, localURL); err != nil {
		return res, fmt.Errorf("setting api_url: %w", err)
	}
	if err := d.set(settings, keyAPIKey, key); err != nil {
		return res, fmt.Errorf("setting api_key: %w", err)
	}
	out := d.bytes()
	st.WrittenSHA256 = digest(out)
	res.Changed = !existed || !bytes.Equal(out, raw)

	// State first: if writing the config then fails, uninstall still
	// knows what to undo; the reverse order could lose the originals.
	if err := m.saveState(st); err != nil {
		return res, err
	}
	if res.Changed {
		if err := writePreservingMode(target, out); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Revert undoes the Install that returned res, for rolling back a failed
// `logbook install`. A first install is undone like Uninstall; a repeat
// install puts back the file bytes and state it replaced, so a failed
// re-install leaves the earlier install exactly as it was.
func (m *Manager) Revert(res InstallResult) error {
	if res.prev == nil {
		_, err := m.Uninstall()
		return err
	}
	if err := m.saveState(res.prev.state); err != nil {
		return err
	}
	if !res.Changed {
		return nil
	}
	if !res.prev.existed {
		_, err := atomicfile.RemoveOptional(res.prev.target)
		return err
	}
	return writePreservingMode(res.prev.target, res.prev.raw)
}

// UninstallResult says what Uninstall did.
type UninstallResult struct {
	Path string
	// NotInstalled means there was nothing to undo.
	NotInstalled bool
	// Restored is "exact" (original bytes), "keys" (only api_url and
	// api_key put back, because the user edited the file since),
	// "removed" (install had created the file) or "" (left alone).
	Restored string
	// BackupKept is the backup's path when it was kept for the user.
	BackupKept string
	// Note explains a file that was left alone.
	Note string
}

// Uninstall puts the config back as it was before Install.
func (m *Manager) Uninstall() (UninstallResult, error) {
	res := UninstallResult{Path: m.path}
	st, ok, err := m.loadState()
	if err != nil {
		return res, err
	}
	if !ok {
		res.NotInstalled = true
		return res, nil
	}
	res.Path = st.Path
	target, err := resolve(st.Path)
	if err != nil {
		return res, err
	}
	raw, exists, err := readBounded(target)
	if err != nil {
		return res, err
	}
	unedited := exists && digest(raw) == st.WrittenSHA256

	switch {
	case !exists:
		res.Note = "the file was deleted since install; nothing to restore"
	case unedited && st.RestoreExact && !st.Existed:
		if _, err := atomicfile.RemoveOptional(target); err != nil {
			return res, err
		}
		res.Restored = "removed"
	case unedited && st.RestoreExact:
		orig, ok, err := readBounded(m.BackupPath())
		if err != nil {
			return res, err
		}
		if !ok {
			if err := m.restoreKeys(target, raw, st, &res); err != nil {
				return res, err
			}
			break
		}
		if err := writePreservingMode(target, orig); err != nil {
			return res, err
		}
		res.Restored = "exact"
	default:
		if err := m.restoreKeys(target, raw, st, &res); err != nil {
			return res, err
		}
	}

	if res.Restored == "exact" {
		if _, err := atomicfile.RemoveOptional(m.BackupPath()); err != nil {
			return res, err
		}
	} else if _, ok, err := atomicfile.ReadOptional(m.BackupPath()); err == nil && ok {
		res.BackupKept = m.BackupPath()
	}
	if _, err := atomicfile.RemoveOptional(m.statePath()); err != nil {
		return res, err
	}
	return res, nil
}

// restoreKeys puts back only api_url and api_key, and only if the file
// still points at Logbook: anything else the user changed stays.
func (m *Manager) restoreKeys(target string, raw []byte, st state, res *UninstallResult) error {
	d := parseDoc(raw)
	cur, _ := d.get(settings, keyURL)
	if !sameURL(cur, st.LocalURL) {
		res.Note = "the file no longer points at Logbook; left unchanged"
		return nil
	}
	if err := restoreKey(d, keyURL, st.Original.URL, st.Original.HadURL); err != nil {
		return err
	}
	if err := restoreKey(d, keyAPIKey, st.Original.APIKey, st.Original.HadKey); err != nil {
		return err
	}
	if err := writePreservingMode(target, d.bytes()); err != nil {
		return err
	}
	res.Restored = "keys"
	return nil
}

func restoreKey(d *doc, key, val string, had bool) error {
	if !had {
		d.remove(settings, key)
		return nil
	}
	if err := d.set(settings, key, val); err != nil {
		return fmt.Errorf("restoring %s: %w", key, err)
	}
	return nil
}

// Installed reports whether the config file currently points at Logbook
// because of an install.
func (m *Manager) Installed() (bool, error) {
	st, ok, err := m.loadState()
	if err != nil || !ok {
		return false, err
	}
	target, err := resolve(st.Path)
	if err != nil {
		return false, err
	}
	raw, exists, err := readBounded(target)
	if err != nil || !exists {
		return false, err
	}
	cur, _ := parseDoc(raw).get(settings, keyURL)
	return sameURL(cur, st.LocalURL), nil
}

// original reads the values Logbook is about to replace. A file that
// already points at the local daemon (an install whose state was lost) has
// no usable original.
func (m *Manager) original(d *doc, localURL string) Original {
	var o Original
	o.URL, o.HadURL = d.get(settings, keyURL)
	o.APIKey, o.HadKey = d.get(settings, keyAPIKey)
	if o.HadURL && sameURL(o.URL, localURL) {
		return Original{}
	}
	if o.APIKey == "" {
		// wakatime-cli reads the legacy apikey when api_key is empty. It
		// stays in the file untouched (api_key takes precedence while
		// Logbook is installed), so HadKey keeps describing api_key only.
		if legacy, ok := d.get(settings, keyLegacyAPIKey); ok {
			o.APIKey = legacy
		}
	}
	if o.APIKey == "" && m.envKey != "" {
		// wakatime-cli falls back to WAKATIME_API_KEY when the file has no
		// key. We keep it for mirroring but don't write it into the file
		// on uninstall (HadKey stays as it was).
		o.APIKey = m.envKey
	}
	return o
}

// backup copies the original file to the data directory once. It never
// overwrites an existing backup. It reports whether uninstall may restore
// the exact original bytes.
func (m *Manager) backup(raw []byte, existed bool) (exact, written bool, err error) {
	if !existed {
		return true, false, nil
	}
	prev, ok, err := readBounded(m.BackupPath())
	if err != nil {
		return false, false, err
	}
	if ok {
		// A backup from an earlier install that was undone by hand. Keep
		// it; it is exact only if it matches what we are replacing now.
		return bytes.Equal(prev, raw), false, nil
	}
	if err := atomicfile.Write(m.BackupPath(), raw, 0o600); err != nil {
		return false, false, fmt.Errorf("backing up .wakatime.cfg: %w", err)
	}
	return true, true, nil
}

// target returns the path to write: the file itself, or what it links to,
// so a dotfiles symlink stays a symlink.
func (m *Manager) target() (string, error) { return resolve(m.path) }

func resolve(p string) (string, error) {
	t, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", p, err)
	}
	return t, nil
}

func (m *Manager) loadState() (state, bool, error) {
	b, ok, err := readBounded(m.statePath())
	if err != nil || !ok {
		return state{}, false, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return state{}, false, fmt.Errorf("parsing %s: %w", m.statePath(), err)
	}
	return st, true, nil
}

func (m *Manager) saveState(st state) error {
	b, err := json.MarshalIndent(st, "", "  ") //nolint:gosec // holds the original key on purpose; written 0600 in the 0700 data dir
	if err != nil {
		return fmt.Errorf("encoding wakatime state: %w", err)
	}
	if err := atomicfile.Write(m.statePath(), b, 0o600); err != nil {
		return fmt.Errorf("saving wakatime state: %w", err)
	}
	return nil
}

// checkImport fails when import_cfg names a file that sets api_url or
// api_key (see ErrImportOverrides).
func checkImport(d *doc) error {
	p, ok := d.get(settings, "import_cfg")
	if !ok || p == "" {
		return nil
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("expanding import_cfg: %w", err)
		}
		p = filepath.Join(home, p[1:])
	}
	raw, ok, err := readBounded(p)
	if err != nil || !ok {
		// wakatime-cli skips a missing import file; so do we. An
		// unreadable one would fail wakatime-cli too, so report it.
		return err
	}
	imp := parseDoc(raw)
	if _, ok := imp.get(settings, keyURL); ok {
		return ErrImportOverrides
	}
	if _, ok := imp.get(settings, keyAPIKey); ok {
		return ErrImportOverrides
	}
	return nil
}

// warnings lists settings that send some heartbeats somewhere other than
// api_url. We leave them alone: they are the user's deliberate routing.
func warnings(d *doc) []string {
	var w []string
	for _, l := range d.lines {
		if l.kind != lineKey {
			continue
		}
		switch l.section {
		case "api_urls":
			w = appendOnce(w, "[api_urls] sends matching projects to other servers; Logbook will not see them")
		case "project_api_key":
			w = appendOnce(w, "[project_api_key] overrides the key for matching projects; Logbook will reject those heartbeats")
		}
	}
	if _, ok := d.get(settings, "api_key_vault_cmd"); ok {
		w = append(w, "api_key_vault_cmd is set; Logbook can only mirror with a key it can read from the file")
	}
	return w
}

func appendOnce(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func readBounded(p string) ([]byte, bool, error) {
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxConfigSize {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", p, maxConfigSize)
	}
	return atomicfile.ReadOptional(p)
}

func writePreservingMode(p string, b []byte) error {
	mode, err := atomicfile.Mode(p, 0o600)
	if err != nil {
		return err
	}
	return atomicfile.Write(p, b, mode)
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sameURL(a, b string) bool {
	return a != "" && strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}
