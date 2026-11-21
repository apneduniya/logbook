# Agent core

The long-running part of the agent (ADR 0009): `logbook run`, the local database, the loopback API, the uploaders, and the `login`, `status` and `doctor` commands.

| Package | Job |
|---|---|
| `internal/agent/localdb` | SQLite database: upload queues, raw events, heartbeat log, key-value state |
| `internal/agent/localapi` | Loopback HTTP server |
| `internal/agent/otlp` | Receives Claude Code's OpenTelemetry active time (mounted in the loopback server) |
| `internal/agent/localkey` | Derives the prompt-hash key and the OTLP token from the local token |
| `internal/agent/upload` | Logbook uploader and Hackatime mirror |
| `internal/agent/daemon` | Wires everything, supervises sensors, drains the hook spool, finds active repos |
| `internal/agent/spoolfile` | Opens the hook spool for appending so the drainer can rename it on every OS |
| `internal/agent/status` | The health snapshot shared by the status page, `/local/v1/status` and `logbook status` |
| `internal/agent/statuspage` | Renders the status page |
| `internal/agent/doctor` | Turns facts about the setup into findings with fixes |
| `internal/agent/account` | Checks an API key against the server |
| `internal/agent/logfile` | Size-rotated log file |
| `internal/agent/version` | Version and User-Agent from build info |

## Files written

All in the data directory (`config.Dir()`: `os.UserConfigDir()/logbook`, or `$LOGBOOK_HOME`):

| File | Mode | Written by | Bound |
|---|---|---|---|
| `config.json` | 0600 | `logbook login` (via `config.Save`) | small |
| `device.key` | 0600 | daemon, on first run (`internal/agent/devicekey`) | small; retired keys kept as `device.key.retired-<unix>` |
| `logbook.db`, `-wal`, `-shm` | 0600 (db) | daemon, `status`/`doctor` when the daemon is down | see retention below |
| `agent.log`, `agent.log.1..3` | 0600 | `logbook run` | 5 MiB × 4 |
| `spool/events.jsonl.<n>.draining` | inherits | daemon (renamed from the hook spool) | removed after draining |

`logbook status` never creates the database: if it doesn't exist yet it reports an empty snapshot.

## Local database

`modernc.org/sqlite` (pure Go), WAL, `synchronous=NORMAL`, `busy_timeout=5000`, `BEGIN IMMEDIATE` transactions, at most 4 connections and a 1 MiB page cache. Migrations are embedded SQL files (`localdb/migrations/NNNN_name.sql`), applied in order, each in its own transaction, recorded in `schema_migrations`. They are forward-only; an agent that finds a newer schema refuses to start (`ErrNewerSchema`).

| Table | Holds | Bound |
|---|---|---|
| `heartbeat_queue` | heartbeats waiting for Logbook (JSON) | 200 000 rows; oldest dropped, counted in `kv` `dropped.heartbeat_queue` and logged |
| `mirror_queue` | heartbeats waiting for Hackatime | same, `dropped.mirror_queue` |
| `heartbeat_log` | time, project and **directory** of each accepted heartbeat (never the file name) | 48 hours |
| `ai_events`, `activity_samples`, `git_events` | raw local evidence (part of the event log), never uploaded. `ai_events.prompt_hash` holds the keyed prompt hash ([harness.md](harness.md#prompt-hashes)) | 90 days and 3 000 000 rows each, sealed rows only (below) |
| `active_time` | Claude Code's active time as deltas (see below; part of the event log) | 90 days and 3 000 000 rows, sealed rows only |
| `heartbeats` | each accepted heartbeat in full, deduplicated, for the struggle engine and evidence (part of the event log; `docs/agent/crediting.md`) | 90 days and 3 000 000 rows, sealed rows only |
| `repo_map` | repository path to project, a row when a path is first seen or its project changes (part of the event log) | a sealed row goes once a newer row for its path is 90 days old |
| `credited_minutes`, `final_spans`, `evidence_days` | the engine's minutes with their computation keys, finality and upload state, and the signed evidence chain (`docs/agent/crediting.md`) | 90 days; one row per day |
| `kv` | uploader state, drop counters, spool progress, device and evidence state | small |

Commits are unique per `(project, commit_hash)`, so the git sensor can report a commit repeatedly. Pruning runs at start and every 6 hours.

The raw event tables (`heartbeats`, `ai_events`, `activity_samples`, `git_events`, `active_time`) and `repo_map` form one append-only event log (ADR 0017, migration 0007): every stored row takes the next sequence number `seq` from `kv` `log.seq` inside its inserting transaction, an ignored duplicate (a resent heartbeat, a commit seen again, a retried export) takes none, and logged rows are never updated. Readers take a snapshot (`DB.View`: one read transaction that reads the high-water mark first) and read rows at or below it. Pruning (90 days) and the row cap delete only rows the device's evidence chain has sealed (`kv` `log.sealed`); when unsealed rows fill a table, new activity samples are dropped and counted (`log.dropped_samples`), and every other kind of row is kept. Any transaction that prunes or trims logged rows also moves `kv` `events.generation`, so in-memory copies of the window know rows are gone.

## Loopback API

Listens on `cfg.LocalAddr`, which must be `127.0.0.1:<port>` or `localhost:<port>` (pinned to `127.0.0.1`). Every request must:

- come from a loopback remote address;
- carry `Host: 127.0.0.1:<port>` or `localhost:<port>` (DNS-rebinding defence);
- not carry an `Origin` other than `http://127.0.0.1:<port>`/`http://localhost:<port>`, nor `Sec-Fetch-Site: cross-site|same-site` (CSRF defence).

Otherwise it gets `403 {"error": ...}`.

| Route | Auth | Behaviour |
|---|---|---|
| `POST /api/v1/users/current/heartbeats(.bulk)`, same under `/api/hackatime/v1` | the Logbook API key, as wakatime-cli sends it (`Basic base64(key)`, also `Bearer`, `api_key=`) | Same request and response shapes as the server (`docs/api/ingest.md`): single → `201` (`/api/v1`) or `202` (`/api/hackatime/v1`); bulk → `201` with one `[body, status]` per item, `400` for invalid items. Valid heartbeats are queued and answered immediately; nothing waits on the network. `X-Machine-Name` and the `User-Agent` are folded into the heartbeat body (the server's attribution rule) so they survive the upload. If the database write fails the answer is `500`, so wakatime-cli keeps the heartbeats in its own offline queue. |
| `GET .../users/current/statusbar/today` | API key | Proxied to `<server>/api/v1/users/current/statusbar/today` with a 3 s timeout and a 30 s cache (failures are cached too, so a dropped network doesn't slow every poll). Only one upstream request is in flight; other polls meanwhile get the last answer or the local estimate at once. If the server can't be reached, rejects the key or answers badly, the agent answers `200` with a WakaTime-shaped body computed from today's local heartbeat log (gaps up to 2 minutes, like the server) and the text suffixed with `(offline)` or `(re-login needed)`. |
| `POST /local/v1/events` | `X-Logbook-Token` = `cfg.LocalToken`, constant-time compare | JSON array of `wire.LocalEvent`, ≤ 1 MiB and ≤ 500 events. Invalid events are counted and skipped; valid ones are stored in one transaction. Answer: `200 {"accepted": n, "rejected": m}`. |
| `POST /v1/metrics` | `X-Logbook-Token` = the OTLP token (derived from the local token, see [harness.md](harness.md#claude-code-telemetry-active-time)) | OTLP/HTTP JSON metrics from Claude Code. Keeps `claude_code.active_time.total` only (see below). `200` with `{}` or an OTLP `partialSuccess` naming rejected datapoints; `415` for protobuf (logged once) or an encoding other than gzip; `413` over 1 MiB (also after gunzip); `503` if storing fails, which the exporter retries. |
| `POST /v1/logs` | same | Accepted and discarded (`200 {}`). The installer never sends logs here. |
| `GET /` | none | Status page: server-rendered HTML, inline CSS, no scripts, `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'`. |
| `GET /local/v1/status` | none | `status.Snapshot` as JSON. |

The status page and JSON never contain the Logbook key, the Hackatime key or the local token.

### Claude Code active time

From each `/v1/metrics` request the receiver keeps only `claude_code.active_time.total` sum datapoints, and of those only `session.id`, `type` (`user` or `cli`), `vcs.repository.name` (present only if the user enabled it), the start and end times and the value. Attributes are read from the datapoint, falling back to the resource. Every other attribute (`user.email`, `user.account_uuid`, `user.account_id`, `user.id`, `organization.id`, `terminal.type`, `host.*`, `app.*` and anything custom) is skipped by the decoder without being read; a test plants a sentinel in all of them and searches the database files and the log.

- Delta datapoints (Claude Code's default) are stored as they are; zero increments are skipped.
- Cumulative datapoints are turned into increments per (`session.id`, `type`, start time) series, from the series' last stored point: the first point counts from the series start; a resent or older point is skipped; a lower total with the same start is a counter reset and counts in full; a new start time is a new series. The running total is stored with each point, so a restarted agent continues the series without double counting.
- Datapoints without a session or a known type, ending in the future (over 2 min), starting more than 90 days ago (a start time of 1970 would otherwise allow decades of seconds), or with more seconds than their interval allows (plus 1 min) are rejected and reported as a partial success.
- A retried export is stored once (unique on session, type and interval).

`localdb.ActiveTimes(ctx, from, to)` returns the points overlapping the window (zero-second points left out) as `struggle.ActiveTime`, sorted, with `ProjectCwd` set to the latest working directory Claude Code's hooks reported for that session (same `session.id`), or empty. Active time is never uploaded.

Claude Code sends nothing for idle intervals, so missing exports would look like zero active time and could make `otel_mismatch` flag an honest session. The daemon therefore records when its receiver runs (table `otlp_uptime`: one row per run, its start and a mark every 30 s, kept 90 days). The first mark is written before the receiver accepts anything. `ActiveTimes` returns a session only when the receiver ran all through the session's span in the window (its Claude Code hook events and its points), counting each run from 2 minutes before its start (an export carries up to a minute of earlier time, and the exporter retries) to 1 minute after its last mark. A session that overlaps a time the agent was stopped is left out entirely, so the engine never judges it on incomplete data.

## Uploads

Two workers share one `http.Client` (30 s total timeout, 10 s dial and TLS, `HTTP(S)_PROXY` honoured) and send `User-Agent: logbook-agent/<version> (<os>-<arch>)`.

| Worker | Queue | Endpoint | Auth |
|---|---|---|---|
| `logbook` | `heartbeat_queue` | `<server_url>/api/v1/users/current/heartbeats.bulk`, with `X-Logbook-Source: agent` | `Basic base64(api_key)` |
| `hackatime` | `mirror_queue` | `<hackatime.url>/users/current/heartbeats.bulk` (the saved `api_url`, e.g. `https://hackatime.hackclub.com/api/hackatime/v1`) | `Basic base64(hackatime.api_key)` |

Heartbeats are queued for the mirror only while `hackatime.mirror` is true and a URL and key are saved.

Each worker wakes every 10 s, or immediately when a full batch (100) is waiting, and sends batches of up to 100, up to 50 batches per wake. Per-item results:

| Answer | Action |
|---|---|
| `201`, `202`, `409` item | delete |
| other 4xx item | delete, count in `upload.<name>.rejected`, log |
| whole request `413`, or `400` with a JSON `{"error"}` body (how both servers refuse a malformed or oversized batch) | split: retry at once with half the batch, down to one heartbeat; a single heartbeat refused this way is deleted and counted, then the worker backs off. The batch size grows back after successes |
| `401` / `403` | keep, pause the worker and show "re-login needed" until the configured key changes; every 30 min one batch is retried with the same key (a `403` may come from a school web filter), and an accepted request resumes the worker |
| `429`, `5xx` (item or batch), network error | keep, back off exponentially from 5 s to 10 min with equal jitter; a longer `Retry-After` wins (capped at 30 min) |
| any other whole-request status (`404` from a stale `api_url`, `405`, `408`, a non-JSON `400` from a proxy), a `3xx`, or a `2xx` without a per-item `responses` array of the batch's length (a captive portal's login page) | keep, back off as above, show the error. Nothing is ever deleted without a per-item answer |

The shared HTTP client follows redirects only for `GET`/`HEAD`; a redirected upload `POST` is not turned into a `GET`, so a captive portal shows up as a `3xx` error. A worker whose saved URL isn't an absolute http(s) URL is shown as paused with a fix instead of sending.

Last success, last error and pause reason are saved in `kv` (`upload.<name>.*`), so `logbook status` shows them with the daemon stopped. A starting worker rewrites the pause reason on its first cycle, and a successful upload clears the last error, so a problem fixed while the daemon was down (e.g. `logbook login` after a rejected key) doesn't linger in status. On shutdown each worker makes one more attempt, bounded to 5 s, unless it is backing off.

## Daemon

`daemon.Run` binds the listener first; the port is the single-instance lock. If it's taken and an agent answers `/local/v1/status` there, `Run` returns `ErrAlreadyRunning` and `logbook run` exits 0. Then it opens the database and runs, under one `errgroup`: the API server, both workers, the spool drainer (at start and every minute), pruning (at start and every 6 h), the crediting pipeline (every minute, supervised like a sensor; `docs/agent/crediting.md`) and each sensor.

Sensors are supervised: a sensor that returns or panics is logged (with a truncated stack) and restarted after 1 s, doubling to 5 min; a run longer than a minute resets the backoff. Sensors receive the database as their `Sink`.

`config.json` is re-read when its modification time changes (checked at most every 5 s), so `logbook login` takes effect without a restart. A config that fails to load is ignored. A changed `local_addr` needs a restart.

### Hook spool

Hooks append to `spool/events.jsonl` when the daemon is down. The drainer:

1. renames it to `events.jsonl.<unix-nanos>.draining`, so new appends start a fresh file;
2. waits 2 s for any hook that opened the old file just before the rename;
3. reads it line by line (lines over 1 MiB and invalid events are skipped and counted), storing 500 events per transaction together with `kv` `spool.offset = <file>:<byte offset>`;
4. deletes it.

A crash mid-drain resumes from the saved offset, so events are neither lost nor duplicated. Leftover `.draining` files are processed first, oldest first.

The first drain runs at start, and crediting waits for it (at most 2 minutes) before its first run and makes no minute final until it has finished: spooled events arrive late, and must count before the minutes they belong to are final (ADR 0017).

Hooks append through `internal/agent/spoolfile`: one write per hook (so lines from concurrent hooks never interleave), at the end of the file, capped at 10 MiB, file 0600 in a 0700 directory. The rename in step 1 has to work while hooks hold the file open:

- **macOS and Linux** rename open files freely; a hook's open descriptor follows the file to its new name.
- **Windows** refuses to rename a file while any handle to it lacks `FILE_SHARE_DELETE`, and `os.OpenFile` never sets it. So hooks open the spool with `CreateFile(FILE_APPEND_DATA, FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE, OPEN_ALWAYS)`. The handle then follows the rename exactly as on POSIX, the grace period covers it, and no hook ever waits for the daemon. Mode bits are advisory on Windows; the file inherits the data directory's ACL.
- Other programs (antivirus, backup tools) may still open the spool without delete sharing. The drainer retries a sharing violation after 10, 20, 40 and 80 ms, then gives up until the next drain a minute later. Nothing is lost meanwhile: hooks keep appending to the live file.

Per-writer segment files were rejected: they would also avoid the rename conflict, but make the 10 MiB cap a directory-wide sum each hook has to compute, and leave many small files behind.

### Active repos (`RepoSource`)

`daemon.NewRepos(db, nil)` lists directories seen in the last hour in heartbeats (the entity's directory) and AI events (`cwd`), most recent first (at most 200), walks each up to the nearest directory containing `.git` (directory or file, so worktrees and submodules count; at most 64 levels), and returns up to 50 repos. `Project` is the project name the heartbeat or AI event used, or else the repo directory's name. Lookups are cached for 10 minutes.

## Commands

| Command | Behaviour |
|---|---|
| `logbook run [--quiet] [--debug]` | Runs the daemon in the foreground. Logs (`log/slog` text) go to `agent.log` and, unless `--quiet`, best-effort to stderr. |
| `logbook login --key <key>\|- [--server <url>]` | Reads the key (from stdin with `-`, keeping it out of shell history), checks it with `GET /api/v1/users/current/statusbar/today`, and only then saves it, creating the config with `config.Default()` if needed. Other config fields are kept. Never prints the key. If `.wakatime.cfg` points at the agent but still holds a different key, says to run `logbook install` (the local API would answer editors 401 until then). |
| `logbook status [--json]` | Asks the daemon; if it's down, reads the database directly. |
| `logbook doctor [--json]` | Checks config, key validity (server), clock skew against the server's `Date` header (warns above 2 min), whether the daemon answers, whether `~/.wakatime.cfg` (or `$WAKATIME_HOME/.wakatime.cfg`) has `api_url` pointing at the agent (`http://127.0.0.1:<port>/api/v1` or `/api/hackatime/v1`, after stripping a trailing slash and legacy endpoint suffixes the way wakatime-cli does) and the same `api_key`, queue health (re-login needed, dropped rows, items older than 1 h) and the spool size. Prints a fix for every problem and exits 1 if any check fails. |

## OS-specific notes

The core has no OS-specific code paths and builds with `CGO_ENABLED=0` for windows, darwin and linux on amd64 and arm64. Behaviour that differs by OS:

- **Windows:** file modes are advisory; `config.Dir()` is `%AppData%\logbook`. `os.Rename` replaces existing files (log rotation relies on this). Rotation fails while another process holds `agent.log` without delete sharing (antivirus, a user tailing it); the writer then reopens the live file, notes the failure once in the log and retries a minute later, so logging never stops. Hooks open the spool with delete sharing so the drainer can rename it (see Hook spool). Stderr may be unusable when started from the `Run` key, so logging to it is best effort.
- **macOS:** `config.Dir()` is `~/Library/Application Support/logbook`.
- **Linux:** `config.Dir()` is `$XDG_CONFIG_HOME/logbook` (default `~/.config/logbook`).

## Performance

`ActiveTimes` over a 2-hour window with three parallel sessions (720 points) and 20 000 hook events takes about 6 ms on the dev VPS under load (`BenchmarkActiveTimes2h`); decoding and storing one export about 1 ms (`BenchmarkExport`).

Idle, the daemon holds one listener, two 10 s timers, a 1-minute spool timer and a 6-hour prune timer. Measured on linux/amd64 with no sensors: about 14 MB RSS and no measurable CPU. Heartbeat writes are batched per request; spool drains 500 events per transaction.
