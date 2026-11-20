//go:build !windows

package selfinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// deleteLater deletes path now: Unix lets a running binary be unlinked.
func deleteLater(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}
