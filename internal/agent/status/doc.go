// Package status builds the agent's health snapshot: which server it talks
// to, whether it is signed in, queue sizes, uploader state, installed hooks
// and today's local counts. The status page, GET /local/v1/status and
// `logbook status` all show the same Snapshot. Collect reads only the local
// database and config, so the CLI can build a snapshot when the daemon is
// down. A Snapshot never contains API keys or the local token.
package status
