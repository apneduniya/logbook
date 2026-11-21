# Devices, credited minutes and evidence API

How the Logbook agent registers its device key and uploads credited minutes and the daily evidence chain (ADR 0010, ADR 0014), and how users manage their devices. Packages: `internal/server/devices`, `internal/server/credited`, `internal/server/ledger`.

## Conventions

- **API-key auth** is the same as for heartbeats (`Authorization: Bearer`, `Basic`, or `?api_key=`; see [auth.md](auth.md)). A bad key gets `401 {"error": "invalid API key"}`.
- **Session auth** is the browser session; `DELETE` also needs `X-CSRF-Token`.
- Errors are `{"error": "message"}`. Bodies are JSON, at most 1 MiB.
- These agent endpoints get no CORS headers: browsers never call them.
- **Rate limits** are per user. Over a limit the response is `429` with `Retry-After`.
  - `POST /api/v1/devices`: a burst of 30, refilled at 30 per hour.
  - `POST /api/v1/devices/{id}/minutes`: a burst of 120, refilled at 2 per second (the heartbeat budget).
  - `POST /api/v1/devices/{id}/evidence-days`: a burst of 60, refilled at 60 per minute.
- A `{id}` that isn't a UUID, doesn't exist, or belongs to another user is `404 {"error": "device not found"}`. The response doesn't say which.
- A revoked device gets `403 {"error": "device has been revoked"}` on every upload. The agent should stop uploading with it and register a new key.

---

## `POST /api/v1/devices`

Auth: API key. Registers the agent's ed25519 public key.

```json
{ "name": "Fiona's laptop", "os": "darwin", "public_key": "<standard base64 of the 32-byte key>" }
```

- `name`: 1–64 characters after trimming, no control or bidirectional-override characters.
- `os`: 1–32 characters, same rules.

| Status | Body | When |
|---|---|---|
| `201` | `{"id": "<uuid>"}` | New device. Audited as `device.register`. |
| `200` | `{"id": "<uuid>"}` | This user already registered this key. Name and OS are not changed. A revoked device stays revoked. |
| `400` | error | Invalid field or JSON. |
| `409` | `{"error": "public key is registered to another account"}` | The key belongs to another user. |
| `409` | `{"error": "you already have 20 active devices; revoke one first"}` | Device cap reached. |

## `POST /api/v1/devices/{id}/minutes`

Auth: API key. Uploads labelled minutes from `pkg/struggle`.

```json
{
  "rules_version": "v1",
  "minutes": [
    { "minute": "2026-11-20T10:31:00Z", "project": "logbook", "label": "you",
      "credited": 45, "cuts": [], "flags": ["low_signal"],
      "computed_at": "2026-11-20T11:00:00Z", "snapshot": 18234 }
  ]
}
```

`computed_at` and `snapshot` are the computation key that produced the row (ADR 0017): the end of the engine's window (RFC 3339 UTC, a whole minute) and the high-water mark of the device's event log the run read. Every row of one minute carries the same key. A step-5 re-run computes the minute again at that key. A changed key is a rewrite, under the finality rule below.

Whole-request errors (`400`): invalid JSON, more than 1000 minutes, or a `rules_version` the server doesn't know (currently only `v1`). An empty `minutes` array is valid and changes nothing; the agent sends one to read the server's clock (`server_time`) before it seals a day.

Each item is then accepted or rejected on its own. `200`:

```json
{ "accepted": 999, "rejected": [{ "index": 3, "error": "minute is in the future" }],
  "server_time": "2026-11-20T10:31:05.123Z" }
```

`server_time` is the server's clock when it checked the batch, RFC 3339 in UTC (fractional seconds optional). The agent seals an evidence day only when this is within 5 minutes of its own clock, and reads it from the body rather than the `Date` header, which a proxy may drop or rewrite. An agent that gets no `server_time`, or one it can't parse, keeps uploading minutes but seals nothing.

`rejected` is sorted by index and is `[]` when empty. When an item is refused because its minute is final, the rejection also carries every row the server holds for that device-minute, which is the server's answer to which value won:

```json
{ "index": 0, "error": "minute is final: ...",
  "stored": [{ "project": "logbook", "label": "you", "credited": 45, "cuts": [], "flags": ["low_signal"],
               "computed_at": "2026-11-20T11:00:00Z", "snapshot": 18234 }] }
```

`stored` is `[]` when the server holds no row for the minute, and `computed_at` and `snapshot` are `null` for a row stored before keys existed. The agent replaces its rows for the minute with exactly these and treats the minute as final and settled. Other rejected items are not retried while their minute is open; the agent sends a final minute again until the server accepts it or answers with its rows. The rules:

| Rule | Error |
|---|---|
| `minute` is RFC 3339 in UTC (`Z` or `+00:00`) on a whole minute | `minute must be an RFC 3339 UTC time on a whole minute` |
| `minute` is at most 2 minutes ahead of the server clock | `minute is in the future` |
| `minute` is in 2024 or later | `minute is before 2024` |
| `project` is valid UTF-8 and at most 256 bytes after normalisation, which is the same as for heartbeats: control characters are dropped, surrounding space is trimmed and `<<LAST_PROJECT>>` becomes `""` (no project). Two items whose projects normalise to the same name are duplicates. | `project must be ...` |
| `label` is `you`, `agent` or `idle` | `label must be ...` |
| `credited` is 0–60, and 0 unless `label` is `you` | `credited must be ...` |
| `cuts` and `flags` have at most 16 codes each, matching `[a-z][a-z0-9_]{0,31}` (duplicates are dropped) | `cuts and flags must be ...` |
| `flags` never contains `multi_device`; the server works that out | `the multi_device flag is set by the server only` |
| The same `(minute, project)` appears only once per request; later copies are rejected | `the same minute and project appear earlier in this batch` |
| `minute` is on or after the UTC day the device was registered. A new key can't write history from before it existed. | `minute is before the UTC day the device was registered` |
| `minute` is not in a UTC day for which this device has stored an evidence record. A sealed day takes no new minutes. | `minute is in a UTC day this device has already sealed ...` |
| A minute is final once it is 30 minutes old by the server clock. A stored `(minute, project)` can only change while the minute is open. A final minute takes no new projects either, once the device has stored any row for it. Re-sending exactly the stored values is always accepted. A minute that was never sent can be sent at any time, so an agent that was offline can catch up. If its catch-up splits one minute across batches, more projects of that minute are accepted for 5 minutes after its first upload, but only if all of its rows were first uploaded after it was final. | `minute is final: ...` |
| One minute has at most 8 projects on one device, whatever their credit. | `a minute can have at most 8 projects per device` |
| Across all projects of one minute on this device, `credited` adds up to at most 60. Removals are applied first, then items that lower a minute's credit, then the rest in request order, so replacing a project or moving a minute between projects works in one request. | `credited seconds for this minute would exceed 60 across projects` |

**Removals.** An item labelled `idle` with `credited` 0 and empty `cuts` and `flags` is a removal: it deletes the stored `(minute, project)` row instead of storing one, freeing that project's slot and credit. The engine never emits a row of that shape (its idle rows always carry a cut), and a missing row already means idle. A removal follows the rules above (a final minute can't lose a row; re-sending a removal is always accepted), and a removal of a row that isn't stored is accepted and changes nothing. When the engine's top 8 projects of an open minute change, the agent sends the displaced project's removal and its replacement in one request, removal first.

- Writes are idempotent upserts keyed by `(device, minute, project)`; removals are idempotent deletes.
- Uploads for one device are serialised, so two concurrent batches can't break the 60-second rule.
- Accepted changes send a `minutes` event on `/api/v1/me/live`.

## `POST /api/v1/devices/{id}/evidence-days`

Auth: API key. Uploads one link of the device's evidence chain (ADR 0010; formats in `pkg/evidence`).

```json
{
  "day": "2026-11-19",
  "prev_chain_hash": "<64 hex, or \"\" for the device's first day>",
  "content_hash": "<64 hex>",
  "chain_hash": "<64 hex>",
  "signature": "<standard base64 of the 64-byte ed25519 signature over the chain hash>",
  "rules_version": "v1",
  "event_counts": { "activity": 9120, "heartbeat": 640, "ai": 75 }
}
```

- The record for `day` covers the device's logged rows from where its previous record ended up to the log's high-water mark when the agent sealed it (ADR 0017): a block can hold events of any time, and every row is sealed exactly once. `content_hash` is `evidence.ContentHash` of those rows; `event_counts` counts them by type (`activity`, `active_time`, `ai`, `git`, `heartbeat`, `repo`).
- Hex is lowercase. `day` is a UTC date, not after today and not before the UTC day the device was registered (`400` otherwise).
- `rules_version` must be one the server knows (currently only `v1`), as for minutes, because step 5 re-runs the engine at that version (`400` otherwise).
- `event_counts` has at most 32 keys matching `[a-z][a-z0-9_.]{0,63}`, with counts from 0 to 10^9.

| Status | When |
|---|---|
| `201` `{"day": "..."}` | Stored. |
| `200` `{"day": "..."}` | An identical record (every field, including the signature and counts) is already stored. |
| `400` | A field is malformed, `chain_hash` isn't `evidence.ChainHash` of the fields, or the signature doesn't verify with the device's registered key. |
| `409 {"error": "a different record is already stored for this day"}` | A different, validly signed record exists for the day. Nothing is changed, and the conflict is written to the audit log as `evidence_conflict` with both versions' hashes. |
| `409 {"error": "chain mismatch"}` | `prev_chain_hash` isn't the `chain_hash` of the device's latest stored day before `day` (or `""` when there is none), or a later day is already stored, so this day can't be inserted before it. |

Gaps are allowed: if nothing was logged since the 17th's record when the 18th is sealed, the 19th links to the 17th.

---

## `GET /api/v1/me/devices`

Auth: session. `200`, newest first, revoked devices included:

```json
{
  "devices": [
    { "id": "…", "name": "Fiona's laptop", "os": "darwin",
      "public_key": "<base64>", "key_fingerprint": "66687aadf862bd77",
      "created_at": "2026-11-20T10:00:00Z", "last_seen_at": "2026-11-20T10:31:00Z",
      "revoked_at": null }
  ]
}
```

- `key_fingerprint` is the first 16 hex characters of the SHA-256 of the public key, so users can match a device to a machine.
- `last_seen_at` is the last upload, updated at most once a minute.

## `DELETE /api/v1/me/devices/{id}`

Auth: session + CSRF. Revokes the device: future uploads with it get `403`. Its credited minutes and evidence are kept. Audited as `device.revoke`.

`204`, or `404` if the device doesn't exist, belongs to another user, or is already revoked.
