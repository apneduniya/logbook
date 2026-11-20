// Package jobs runs the server's background jobs on River (ADR 0002). It owns
// River's lifecycle (building the client, Start/Stop) and River's own schema
// migrations. It knows nothing about individual jobs: feature packages define
// their job args and workers and hand them in through Options.
package jobs
