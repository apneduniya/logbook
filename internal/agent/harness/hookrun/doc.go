// Package hookrun runs one `logbook hook <harness> <event>` invocation: read
// the payload from stdin (capped and time-limited), turn it into AI events,
// print the harness's required stdout response, and hand the events to the
// local daemon or the spool. It never fails the harness: every error is
// swallowed or logged to the agent log, and nothing but the protocol
// response is written to stdout or stderr.
package hookrun
