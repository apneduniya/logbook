//go:build windows

package autostart

import "os"

// New returns the autostart manager for this OS.
func New(Options) *Windows {
	return NewWindows(RegistryRunKey{}, ExecRunner{}, DetachedSpawner{}, os.Getpid())
}
