# 0010: Minute crediting, device keys and the signed evidence chain

- Status: accepted (the submission part is replaced by ADR 0020; the content hash by ADR 0019)
- Date: 2026-11-20

## Context

The bounty asks for hours of productive struggle that an outside party can audit. ADR 0006 puts the struggle engine on the device. This ADR fixes the remaining details:
- what the engine outputs;
- how the server stores credited time next to raw (Hackatime-style) time;
- how evidence is made tamper-evident.

The rules themselves are in `docs/STRUGGLE-RULES.md`.

## Decision

**Engine.** `pkg/struggle` is pure and deterministic: the same `Input` and `Rules` version always give the same `[]Minute`. Its types are in `pkg/struggle/types.go`. The agent runs it every minute over a sliding window (the last 60 minutes, so late events are included). A minute is final once it's 30 minutes old.

**Raw time and credited time are kept separate on the server.**
- `user_minutes` stays the raw, Hackatime-equivalent rollup from heartbeats. It's what `total_seconds` means for compatibility.
- A new `credited_minutes` table holds the agent's labelled minutes, keyed by user, device, minute and project. It stores label, credited seconds, cut and flag codes, and rules version.
- Stats show both. A user without the agent has raw time only and no evidence; programs can see that.

**Device keys.**
- At install or login the agent generates an ed25519 key pair. The private key is stored in the data dir with mode 0600.
- It registers the public key with `POST /api/v1/devices` (API-key auth). The server returns a device id.
- Devices can be listed and revoked by the user. Revoking stops future uploads but keeps the history.

**Evidence chain.** At the end of each UTC day, plus a 1-hour grace period for late events:
- `content_hash = SHA-256(canonical encoding of the day's raw events)`. The canonical encoding is JSON lines of `wire` types, sorted by (time, type, stable id), defined in `pkg/evidence`.
- `chain_hash = SHA-256("logbook-evidence-v1" ‖ device_id ‖ day ‖ prev_chain_hash ‖ content_hash ‖ rules_version)`.
- The chain hash is signed with the device key.
- The agent uploads `{day, content_hash, prev_chain_hash, chain_hash, signature, rules_version, event_counts}` to `POST /api/v1/devices/{id}/evidence-days`.
- The server checks the signature and that `prev_chain_hash` matches the last stored day, and stores it. It never accepts a second, different hash for the same day.
- A gap in the chain (the agent was off) is allowed and recorded. A conflicting value is rejected and flagged.

**Minute uploads.** `POST /api/v1/devices/{id}/minutes` takes batches of up to 1000 minutes and upserts them idempotently. Rewrites are allowed only for minutes younger than 30 minutes; later writes to the same minute are rejected.

**Submission (step 5).**
- The user picks a project and date range.
- The agent bundles that range's raw events plus the evidence-day records.
- The server checks every day's `content_hash` against the bundle, re-runs `pkg/struggle` at the recorded rules version, and compares the result with the uploaded credited minutes.
- Any difference is flagged.

## Consequences

- Evidence can't be changed after the day closes without breaking a signature or the chain.
- The server never sees raw activity unless the user submits it.
- A modified agent could still fabricate events in real time; the flags and reviewers cover that, and the threat model accepts the residual risk.
- Raw and credited time give reviewers an honest picture: "Hackatime would say 10 h; 7.5 h is credited, with these cuts."
