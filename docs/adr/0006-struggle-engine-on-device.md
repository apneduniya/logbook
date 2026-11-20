# 0006: The struggle engine runs on the device; the server gets labelled minutes and a signed hash chain

- Status: accepted
- Date: 2026-11-20

## Context

To credit productive struggle, Logbook needs detailed signals: activity timing, AI harness events and git activity. Privacy (requirement 6) rules out uploading them. Auditability (requirement 10) means the evidence has to be trustworthy after the fact.

## Decision

- **Rules engine:** `pkg/struggle` is a pure Go package with no I/O. Its input is a stream of events; its output is per-minute labels (`you`, `agent`, `idle`), plus the list of cuts with their reasons. It has a version number and golden tests.
- **What the agent keeps locally:** raw events in a SQLite database (`modernc.org/sqlite`, pure Go, no cgo). The agent runs the engine on them.
- **What the agent uploads:**
  - labelled minutes per project;
  - for each day, a SHA-256 hash of that day's canonical evidence, chained to the previous day's hash and signed with the device's ed25519 key. The key is registered with the server when the device is paired.
- **Submission:**
  - The user reviews the bundle, which covers only the chosen project and date range, then uploads it.
  - The server checks it against the stored hash chain and re-runs the engine at the bundle's rules version.
  - If the server's result differs from what the agent reported, the submission is flagged.

## Consequences

- Raw activity never leaves the device unless the user submits it.
- Evidence can't be changed after the fact without breaking the hash chain.
- The server can recompute credited time on its own to check the agent's numbers.
- A modified agent can still invent events in real time. Server-side plausibility checks and reviewers' flags cover this, and the threat model accepts that risk.
