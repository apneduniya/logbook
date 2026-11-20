// Package daemon runs the agent's long-lived process, `logbook run`
// (ADR 0009). It wires the local database, the loopback API, the two
// uploaders, the hook spool drainer and the sensors under one context, and
// supervises them: a sensor that returns an error or panics is restarted
// with backoff and never takes the daemon down.
//
// It owns: start-up order, the single-instance guarantee (the listen port is
// the lock), config reloads after `logbook login`, periodic pruning, the
// spool drain, repo discovery for the git sensor (RepoSource), and graceful
// shutdown. It defines the Sensor and Sink interfaces sensors implement
// structurally. It does not own any sensor, the upload protocol
// (internal/agent/upload) or the HTTP handlers (internal/agent/localapi).
package daemon
