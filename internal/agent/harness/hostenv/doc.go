// Package hostenv describes the machine the harness installers work on: the
// home directory, OS, environment variables, PATH lookups, where backups go,
// the local agent's address and token, and where machine-wide policy files
// live (SystemPath). Installers take an Env instead of
// calling the os package directly or reading the agent config, so
// tests can point them at a temp directory and pretend to be any OS.
package hostenv
