// Package spoolfile opens the hook spool (config.SpoolPath) for appending in
// a way that lets the daemon rename it while hooks still have it open, on
// every OS. Hook commands append through it; the daemon's drainer renames
// and reads the file. It does not decide what is written or when.
package spoolfile
