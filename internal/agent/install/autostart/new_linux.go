//go:build linux

package autostart

// New returns the autostart manager for this OS.
func New(o Options) *Linux {
	return NewLinux(o.ConfigHome, ExecRunner{}, DetachedSpawner{})
}
