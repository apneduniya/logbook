// Package e2e holds tests that run the agent's crediting pipeline (local
// database, engine, upload client) against the server's real minutes
// endpoint and Postgres store. It has no code outside its tests, and it is
// the one place that imports both internal/agent and internal/server.
package e2e
