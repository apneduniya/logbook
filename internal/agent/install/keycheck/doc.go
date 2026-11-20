// Package keycheck checks that a Logbook API key is well formed and that
// the Logbook server accepts it, before the installer changes anything. It
// sends the key only to the Logbook server, the way wakatime-cli does, and
// never logs it. It does not store the key; the install command does.
package keycheck
