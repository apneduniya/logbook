// Package router builds the API server's HTTP handler: it mounts every
// package's routes on one mux and wraps it in the global middleware
// (recovery, request ids, logging, security headers and CORS).
//
// It owns the route set as a whole (Go's mux panics when two patterns
// conflict, so building it in one place lets a test catch that), the list
// of /api/ paths that get no CORS because browsers never call them, and the
// adapter that lets the program API check OAuth access tokens (package
// oauth) and user API keys (package auth).
//
// It holds no business rules. Services are built in cmd/logbook-server and
// passed in; each package's handler decides what its routes do.
package router
