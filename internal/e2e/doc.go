// Package e2e holds tests that run the agent's crediting pipeline (local
// event log, engine, upload client) against the server's real minutes and
// evidence-day endpoints and Postgres store. Every test ends by checking
// what ADR 0017 promises: the agent's final minutes, the server's rows and
// a replay of each minute at its computation key agree, and every sealed
// evidence block re-hashes to the content hash the agent signed. The
// evidence tests also check ADR 0019 end to end: the server stores each
// block's signed fields and their seq continuity, and proofs the agent
// builds verify against the server's stored records, prove every window
// complete, and replay (pkg/replay) to the server's stored rows. It has no
// code outside its tests, and it is the one place that imports both
// internal/agent and internal/server.
package e2e
