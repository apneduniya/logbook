//go:build darwin

package autostart

import "os"

// New returns the autostart manager for this OS.
func New(o Options) *Launchd {
	return NewLaunchd(o.Home, o.DataDir, os.Getuid(), ExecRunner{})
}
