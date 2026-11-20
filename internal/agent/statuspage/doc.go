// Package statuspage renders the agent's local status page (GET / on the
// loopback server) from a status.Snapshot. It is server-rendered
// html/template with inline CSS and no scripts or external assets, so it
// works offline and needs nothing from the network. It never renders keys;
// a Snapshot doesn't carry them.
package statuspage
