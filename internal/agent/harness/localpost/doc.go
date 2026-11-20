// Package localpost delivers events from a hook command to the local Logbook
// daemon: a POST of a JSON array of wire.LocalEvent to
// http://<local addr>/local/v1/events with the X-Logbook-Token header and a
// short timeout. When the daemon can't take them (down, slow, wrong token,
// not configured), the events are appended as JSON lines to the spool file,
// which the daemon drains later. The spool is capped; when full, new events
// are refused and logged rather than old ones overwritten.
package localpost
