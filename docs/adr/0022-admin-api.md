# 0022: A read-only admin API for Fraud Squad

- Status: accepted
- Date: 2026-11-25

## Context

Requirement 10 asks for hours "auditable by an external party". Fraud Squad and HQ audit hours after programs approve them, and their tools (Sidekick, Telescreen, Fallen, Ari) read Hackatime's admin API: `user/info`, `user/heartbeats` (paged, `has_more`), `user/projects`, `user/trust_logs` and a few searches (research, 19 Nov).

In the bounty thread, the strongest objection to client-side trackers was that their numbers are self-reported. The answer is that every counted minute can be traced to signed raw events, and Fraud Squad should be able to do that tracing themselves, not take our page's word for it.

## Decision

- **Who:** users with `role = 'admin'`. The role is set only from the server's command line (`logbook-server admin grant <username>` / `revoke`), never through the API. In V1 that's us and whoever HQ names for Fraud Squad.
- **How:** an admin creates admin keys in `/settings` (shown once, stored as SHA-256 hashes, revocable, prefixed `lba_` so they can't be confused with user keys). Requests use `Authorization: Bearer <key>`. A session can't call the admin API, so a stolen browser session can't use it.
- **Read-only in V1.** Bans stay in Hackatime. Nothing here changes a user's data.
- **Every call is audited** (`admin.read`, with the admin, the endpoint and the subject user).
- **Endpoints** under `/api/admin/v1/` (shapes in `docs/api/admin.md`). Where Hackatime has the same call, the path, paging and field names match it, so Fraud Squad tools can point at us:
  - `user/info` (by id, username or email), `user/heartbeats` (raw heartbeats, `limit`/`offset`/`has_more`), `user/projects`;
  - `user/devices`: devices, their evidence chains (days sealed, gaps, any conflicting upload refused);
  - `user/minutes`: credited minutes with label, cuts, flags, rules version and key, paged;
  - `user/submissions` and `submissions/{id}`: every submission with its verification and decisions;
  - `submissions/{id}/evidence`: the revealed raw rows of a submission, decoded, with each row's block and whether it re-hashed;
  - `user/audit_log`.
- **What admins can see that reviewers can't:** raw heartbeats (file paths included) and the revealed rows of other projects inside a submission's replay windows. The privacy page says so.

## Consequences

- Fraud Squad can rebuild any counted minute from the evidence itself, and see every case where the server's re-check disagreed with a device.
- Admin keys are the most powerful credential we issue. They're hashed, revocable, separate from user keys, and every use is logged.
- Write actions (for example marking a submission as fraudulent) are left for after the pilot, once Fraud Squad says what they need.
