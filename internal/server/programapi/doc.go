// Package programapi serves the Hackatime-shaped read API that YSWS programs
// call (ADR 0008, ADR 0021, docs/api/programs.md): authenticated/me,
// authenticated/projects, users/{id}/stats, trust_factor, heartbeats/spans,
// the per-user project endpoints and my/heartbeats.
//
// Paths, parameters, status codes and JSON shapes match Hackatime at commit
// 43bde10. What it adds:
//
//   - total_seconds is counted (credited) time from credited_project_minutes
//     unless raw=true; credited_seconds and raw_seconds sit beside it;
//   - users are named by their numeric public_id (or username);
//   - one extra top-level "logbook" object (username, hackatime_id,
//     trust_verified, notes).
//
// Trust levels come from the user's Hackatime link and are masked as
// Hackatime masks them publicly (trust.go). Hidden projects are invisible to
// programs everywhere.
//
// It does not issue or check OAuth tokens or API keys itself: callers inject
// an Authenticator (backed by packages oauth and auth). It never writes, and
// it does not own submissions, apps or members (later steps).
package programapi
