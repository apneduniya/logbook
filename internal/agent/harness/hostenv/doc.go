// Package hostenv describes the machine the harness installers work on: the
// home directory, OS, environment variables, PATH lookups and where backups
// go. Installers take an Env instead of calling the os package directly, so
// tests can point them at a temp directory and pretend to be any OS.
package hostenv
