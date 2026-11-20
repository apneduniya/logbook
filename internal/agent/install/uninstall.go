package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/install/atomicfile"
)

// UninstallOptions are the `logbook uninstall` flags.
type UninstallOptions struct {
	// Purge also deletes the data directory (config, keys, local database,
	// logs). It needs Yes, because it can't be undone.
	Purge bool
	Yes   bool
}

// ErrPurgeNeedsYes means --purge was given without --yes.
var ErrPurgeNeedsYes = errors.New("--purge deletes all local Logbook data, including events not yet uploaded; add --yes to confirm")

// purgeAttempts bounds retries while Windows releases files held by the
// daemon that was just stopped.
const purgeAttempts = 5

// Uninstall reverses Install in reverse order: it stops the daemon and
// removes autostart, removes harness hooks, restores .wakatime.cfg and
// removes the binary. It keeps going past a failed step so as much as
// possible is undone, and returns every error. The config stays unless
// Purge is set, so a later install keeps the same local token.
func (s *Service) Uninstall(ctx context.Context, o UninstallOptions) error {
	if o.Purge && !o.Yes {
		return ErrPurgeNeedsYes
	}
	var errs []error
	fail := func(step string, err error) {
		s.say(step, "FAILED: %v", err)
		errs = append(errs, fmt.Errorf("%s: %w", step, err))
	}

	if ok, err := s.d.Autostart.Installed(); err != nil {
		fail("autostart", err)
	} else if err := s.d.Autostart.Uninstall(); err != nil {
		fail("autostart", err)
	} else if ok {
		s.say("autostart", "removed %s and stopped the daemon", s.d.Autostart.Path())
	} else {
		s.say("autostart", "not set up")
	}

	for _, h := range s.d.Harnesses {
		ok, err := h.Installed()
		if err != nil {
			fail("hooks", fmt.Errorf("%s: %w", h.Name(), err))
			continue
		}
		if !ok {
			continue
		}
		if err := h.Uninstall(); err != nil {
			fail("hooks", fmt.Errorf("%s: %w", h.Name(), err))
			continue
		}
		s.say("hooks", "%s hooks removed", h.Name())
	}

	keptBackup := ""
	if res, err := s.d.Waka.Uninstall(); err != nil {
		fail("wakatime", err)
	} else {
		s.say("wakatime", "%s", describeRestore(res.Path, res.Restored, res.Note, res.NotInstalled))
		keptBackup = res.BackupKept
		if keptBackup != "" && !o.Purge {
			s.say("wakatime", "your original is still at %s", keptBackup)
		}
	}

	if removed, err := s.d.Binary.Uninstall(); err != nil {
		fail("binary", err)
	} else if removed {
		s.say("binary", "removed %s", s.d.Binary.Path())
	}

	switch {
	case o.Purge && len(errs) > 0:
		// The data dir holds what is needed to retry (the .wakatime.cfg
		// backup and state), so never delete it after a failed step.
		s.say("data", "kept %s because a step above failed; fix it and run uninstall again", s.d.DataDir)
	case o.Purge:
		if keptBackup != "" {
			if err := s.saveBackupBeside(keptBackup); err != nil {
				fail("data", err)
				break
			}
		}
		if err := purge(ctx, s.d.DataDir); err != nil {
			fail("data", err)
		} else {
			s.say("data", "deleted %s", s.d.DataDir)
		}
	default:
		s.say("data", "kept %s (use --purge --yes to delete it)", s.d.DataDir)
	}
	return errors.Join(errs...)
}

// saveBackupBeside copies a .wakatime.cfg backup that uninstall could not
// use out of the data directory before it is purged, so the user's
// original file is never lost. An existing copy is left alone.
func (s *Service) saveBackupBeside(backup string) error {
	dst := s.d.Waka.Path() + ".logbook-backup"
	if _, ok, err := atomicfile.ReadOptional(dst); err != nil || ok {
		if ok {
			s.say("wakatime", "your original is at %s", dst)
		}
		return err
	}
	b, _, err := atomicfile.ReadOptional(backup)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(dst, b, 0o600); err != nil {
		return fmt.Errorf("saving the .wakatime.cfg backup: %w", err)
	}
	s.say("wakatime", "your original is saved at %s", dst)
	return nil
}

func describeRestore(path, restored, note string, notInstalled bool) string {
	switch {
	case notInstalled:
		return "not changed by Logbook"
	case restored == "exact":
		return "restored " + path + " from the backup"
	case restored == "keys":
		return "restored api_url and api_key in " + path + " (your other edits are kept)"
	case restored == "removed":
		return "removed " + path + " (Logbook had created it)"
	default:
		return path + ": " + note
	}
}

// purge deletes the data directory after checking it really is one, so a
// mistaken LOGBOOK_HOME can never delete a home directory or a drive.
func purge(ctx context.Context, dir string) error {
	if err := checkPurgeable(dir); err != nil {
		return err
	}
	var err error
	for range purgeAttempts {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		t := time.NewTimer(400 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return fmt.Errorf("deleting %s: %w", dir, err)
}

func checkPurgeable(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", dir, err)
	}
	if filepath.Dir(abs) == abs {
		return fmt.Errorf("refusing to delete %s: it is a filesystem root", abs)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == abs {
		return fmt.Errorf("refusing to delete %s: it is the home directory", abs)
	}
	if filepath.Base(abs) == "logbook" {
		return nil
	}
	if _, err := os.Stat(config.Path(abs)); err != nil {
		return fmt.Errorf("refusing to delete %s: it does not look like a Logbook data directory", abs)
	}
	return nil
}
