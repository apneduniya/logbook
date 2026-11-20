// Package projectname normalises project names, so a project gets the same
// name whether it arrives in a heartbeat (internal/server/ingest) or in a
// credited minute (internal/server/credited), and the agent can apply the
// same rule before it computes minutes. It is pure: no I/O.
//
// It does not check lengths or decide what a project is; callers do.
package projectname
