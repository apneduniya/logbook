// Package harnesstest holds the shared checks the harness packages' tests
// run: golden install/uninstall round trips for a JSON hook config, and the
// privacy check that no prompt or tool text reaches an AI event. It is only
// imported by _test.go files.
package harnesstest
