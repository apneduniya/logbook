# Threat model: Logbook (20 Nov 2026)

## 1. What we're building

```
 device (untrusted by the server)            │  server (trusted)                │  third parties
                                             │                                  │
 editor plugins ─► wakatime-cli ─► agent ────┼─► API (TLS) ─► Postgres          │
 AI harness hooks ─────────────►   (Go)      │       │                          │
 OS idle/focus, git ───────────►   SQLite    │       ├─► River jobs ────────────┼─► Hackatime (import, OAuth)
                                   + keys    │       ├─► web UI (embedded)      │   HCA (sign-in)
                         └─ mirror ──────────┼───────┼──────────────────────────┼─► Hackatime (heartbeats)
                                             │       └─► program API ◄──────────┼── program backends, reviewers,
                                             │                                  │   Fraud Squad
```

There are four trust boundaries:
- **B1** device ↔ server;
- **B2** browser ↔ server;
- **B3** server ↔ third parties (HCA, Hackatime, GitHub);
- **B4** program clients ↔ program API.

## 2. What can go wrong (STRIDE)

| Boundary | Spoofing | Tampering | Repudiation | Info disclosure | DoS | Elevation |
|---|---|---|---|---|---|---|
| B1 device→server | Stolen API key used to log time for someone else | Modified agent invents minutes; evidence edited after the fact | User denies having sent hours | Raw activity, prompts or code leaving the device | Heartbeat floods | n/a |
| B2 browser | Session theft, OAuth CSRF | CSRF on mutations | Reviewer denies a decision | Private profiles or projects leaking; IDOR on submissions | Expensive dashboard queries | User reaches reviewer or admin actions |
| B3 third parties | Forged OAuth callbacks | Hackatime returns unexpected data during import | n/a | OAuth tokens leaking from the DB | Hackatime rate limits stalling imports | Over-broad scopes |
| B4 programs | Rogue OAuth client | n/a | n/a | A program reading users who never authorised it | Scraping the stats endpoints | A `read` token used for admin data |

## 3. Mitigations

| Threat | Mitigation | Status |
|---|---|---|
| Stolen API key | Per-device keys, stored only as hashes on the server, revocable from the dashboard; the device signs evidence with its own ed25519 key | planned (step 1–2) |
| Invented minutes | Server re-runs the struggle engine on submitted evidence; plausibility checks (overlapping devices, > 24 h/day, implausibly regular patterns); review flags; Hackatime trust level gate | planned (step 4–5) |
| Evidence edited afterwards | Daily signed hash chain uploaded before submission (ADR 0006) | planned (step 4) |
| Repudiation | Append-only audit log for submissions, reviewer decisions, project reassignments, key changes | planned (step 1, 5) |
| Raw data leaving device | Only labelled minutes and hashes are uploaded by default; bundle is previewed by the user; privacy page lists every field sent | planned (step 2, 4) |
| Heartbeat floods / scraping | Per-key and per-IP rate limits; body size limits; bulk batch cap (WakaTime's is 25) | planned (step 1) |
| Session theft / CSRF | Server-side sessions with hashed ids; `HttpOnly; Secure; SameSite=Lax` cookies; CSRF token on all state-changing browser requests; OAuth `state` + PKCE | planned (step 1) |
| IDOR | Every query that reads a user's data is scoped to that user; ownership is checked in the service layer; tests for cross-user access | planned (step 1) |
| Privilege escalation | Roles (`user`, `reviewer` per program, `admin`) are checked in middleware and again in services | planned (step 1, 5) |
| Token leakage | Third-party tokens encrypted with AES-GCM using a key from env; our API keys and OAuth tokens stored as SHA-256 hashes (an open issue on Hackatime); least-privilege scopes | planned (step 1) |
| Import stalls | River retries with backoff, per-user import state, resumable day cursors | planned (step 1) |
| Supply chain | Pinned dependency versions, `go.sum` committed, `govulncheck` and `golangci-lint` in CI, signed release checksums | planned (step 0, 6) |

## 4. Did we do a good job?

Review after each build step and before the pilot.

**Accepted risk:** a determined user with a modified agent can fabricate activity in real time. No passive tracker can fully prevent that; we make it detectable rather than impossible.

## Data handled

- **Hack Club identity:** HCA id, name and email. Email is never shown publicly.
- **Hackatime OAuth tokens:** encrypted at rest.
- **Heartbeats:** project, language, editor, file paths. File paths are hashed for public views.
- **Labelled minutes, submission bundles** (only those the user chose to upload), **reviewer decisions**.

No government IDs and no payment data. Prompts and code reach the server only inside a bundle the user has submitted, and are deleted when the program's review closes plus 90 days.
