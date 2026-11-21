// Package e2e holds tests that run the agent's crediting pipeline (local
// event log, engine, upload client) against the server's real minutes and
// evidence-day endpoints and Postgres store. Every test ends by checking
// what ADR 0017 promises: the agent's final minutes, the server's rows and
// a replay of each minute at its computation key agree, and every sealed
// evidence block re-hashes to the content hash the agent signed. It has no
// code outside its tests, and it is the one place that imports both
// internal/agent and internal/server.
package e2e
