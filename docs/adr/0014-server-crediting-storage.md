# 0014: How the server stores and sums credited minutes

- Status: accepted
- Date: 2026-11-20

## Context

ADR 0010 says the agent uploads labelled minutes and a daily signed evidence record, and that the server shows credited time next to raw time. It leaves open:
- how the tables are laid out and whether `credited_minutes` is partitioned like `heartbeats`;
- what "credited time" means for a user with several devices;
- when the `multi_device` flag is decided;
- what stats show for users who never installed the agent;
- how uploads stay consistent when an agent retries or two uploads race.

## Decision

**Tables** (migration `00003_crediting.sql`):
- `devices`: one row per agent key. The public key is unique across all users. Devices are revoked, never deleted.
- `credited_minutes`: primary key `(device_id, minute, project)`, plus `user_id` with indexes on `(user_id, minute)` and `(user_id, project, minute)`. A composite foreign key to `devices (id, user_id)` guarantees a row's user is its device's owner. Checks: whole minutes, `credited` 0..60, and credit only on `you` minutes.
- `evidence_days`: primary key `(device_id, day)`. Hashes are lowercase hex, as `pkg/evidence` writes them.

**`credited_minutes` is not partitioned.**

| | Partition by month (like `heartbeats`) | One table |
|---|---|---|
| Size | Same rows either way: at most one per device, minute and project, and at most 8 projects per device-minute (enforced on upload). Minutes can't be written before the device's registration day or into a sealed day, so rows grow with real time only. A heavy real user is about 500k rows a year; the hard ceiling is 8 rows per active device per minute (20 active devices per user) | Same |
| Retention | Partitions help when old data is dropped. Credited minutes are evidence and must stay auditable, so nothing is dropped | Nothing to drop |
| Reads | Every read already filters on `user_id` and a time range, so the indexes do the work | Same plans, one table |
| Writes | Upserts on the primary key must include the partition key; the key is already `(device_id, minute, project)`, so it works, but every partition carries its own copy of the indexes | One set of indexes |
| Operations | Partitions must be created ahead of time, as for `heartbeats` (a default partition catches the rest) | Nothing to schedule |

Heartbeats are partitioned because they are many per minute and raw. Credited minutes are at most one per device-minute-project, so one table is simpler and costs the same on the hosting budget. If it ever grows past what an index scan handles, it can be partitioned by `user_id` hash, which matches every query better than time ranges.

**Credited time across devices.** For one user and one minute, credited seconds are summed over all devices and projects. If the sum is over 60, the minute is capped at 60 and each project gets its share: `credited × 60 / total`, rounded down. A user can't earn more than a minute per minute by running the agent on two machines. The rule lives in one place, the `credited_project_minutes` view, and every stats query reads through it.

**`multi_device` is computed when read, not stored.** A minute in which two or more devices credited `you` time is counted in the summary's `multi_device_minutes`. Storing the flag would mean rewriting the other device's rows when a second device uploads, and rows older than 30 minutes are final. Reviewers in step 5 get the same count from the same data.

**No device means no evidence, not zero.** Users who have never registered a device get `null` for every credited field. A user whose devices are all revoked still has history and gets numbers. Raw (heartbeat) totals are unchanged, so Hackatime-compatible clients see exactly what they did before.

**Upload consistency.**
- Each upload runs in one transaction that locks the device row (`FOR NO KEY UPDATE`). Uploads for one device are serialised, so the 60-second rule per device-minute is checked against committed data. A concurrent revoke waits for the upload to finish.
- An existing minute may be rewritten only while it is under 30 minutes old by the server clock (ADR 0010). Finality covers the whole device-minute: once final, it takes no new projects either, so a minute sent as idle can't gain "you" time later under another project. The one exception is an offline catch-up that splits a minute across batches: if every row of the minute was first written after it was final, more projects are accepted for 5 minutes after the first. Sending exactly the stored values again is always accepted, so retries are safe.
- A device writes nothing before the UTC day it was registered (minutes and evidence days alike), and no new minutes into a day it has sealed with an evidence record. A new key therefore can't fabricate signed history for closed days; the agent starts its chain on its registration day too. Minutes an agent computed before signing in on earlier days are not accepted.
- At most 8 projects per device-minute, so rows with no credit can't grow without bound. This is `struggle.MaxProjectsPerMinute`: the engine never emits more, so the check only stops other clients.
- Within a batch, items that lower a minute's credit are applied before the rest, so the agent can move a minute from one project to another in one request.
- Only rules versions the server knows are accepted, for minutes and evidence days, because step 5 re-runs the engine at the stored version.
- Project names are normalised exactly like heartbeat projects (`pkg/projectname`), so raw and credited totals, and profile project hiding, use the same names. The engine (`pkg/struggle`) applies the same `projectname.Normalize` before it counts projects, so a correct agent never sends two spellings of one project for a minute; the server still normalises, as a guard.

**Evidence days.** The signature is checked before anything is compared, so only validly signed conflicts are treated as conflicts. A different record for a stored day is rejected with `409` and written to the audit log (`evidence_conflict`), because it means the device key signed two histories for one day. A day may not be inserted before a stored later day (the later day's `prev_chain_hash` already skips it).

## Consequences

- Credited totals never exceed wall-clock time, whatever the number of devices.
- The cap happens when read, so a device that uploads late changes earlier totals until those minutes are final on that device.
- Integer division can lose up to a second per project per capped minute; totals stay at or under 60.
- Stats run one extra query (`has devices`) per request; it is an index lookup.
