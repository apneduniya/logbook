// Package projectname normalises project names, so a project gets the same
// name whether it arrives in a heartbeat (internal/server/ingest) or in a
// credited minute (internal/server/credited), and the crediting engine
// (pkg/struggle) applies the same rule before it counts projects, so its
// output is already in the server's form. It is pure: no I/O.
//
// It does not check lengths or decide what a project is; callers do.
package projectname
