// Package hostenv describes the machine the harness installers work on: the
// home directory, OS, environment variables, PATH lookups, where backups go
// and the local agent's address and token. Installers take an Env instead of
// calling the os package directly or reading the agent config, so
// tests can point them at a temp directory and pretend to be any OS.
package hostenv
