package autostart

// Options locate the current user's directories. The command fills them
// from os.UserHomeDir, os.UserConfigDir and config.Dir.
type Options struct {
	Home       string // home directory
	ConfigHome string // os.UserConfigDir(): $XDG_CONFIG_HOME or ~/.config on Linux
	DataDir    string // the agent's data directory
}
