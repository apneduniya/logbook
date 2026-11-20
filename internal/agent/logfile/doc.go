// Package logfile is a size-rotated log file for the agent daemon: when the
// file would grow past MaxBytes it is renamed to <name>.1 (shifting older
// copies up to <name>.<Keep>, dropping the oldest) and a new file is
// started. It owns nothing else; what is logged is the caller's business.
package logfile
