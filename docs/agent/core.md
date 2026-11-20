# Agent core

The long-running part of the agent (ADR 0009): `logbook run`, the local database, the loopback API, the uploaders, and the `login`, `status` and `doctor` commands.

| Package | Job |
|---|---|
| `internal/agent/localdb` | SQLite database: upload queues, raw events, heartbeat log, key-value state |
| `internal/agent/localapi` | Loopback HTTP server |
| `internal/agent/upload` | Logbook uploader and Hackatime mirror |
| `internal/agent/daemon` | Wires everything, supervises sensors, drains the hook spool, finds active repos |
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
| `ai_events`, `activity_samples`, `git_events` | raw local evidence, never uploaded in step 2 | 90 days and 3 000 000 rows each |
| `kv` | uploader state, drop counters, spool progress | small |

Commits are unique per `(project, commit_hash)`, so the git sensor can report a commit repeatedly. Pruning runs at start and every 6 hours.

## Loopback API

Listens on `cfg.LocalAddr`, which must be `127.0.0.1:<port>` or `localhost:<port>` (pinned to `127.0.0.1`). Every request must:

- come from a loopback remote address;
- carry `Host: 127.0.0.1:<port>` or `localhost:<port>` (DNS-rebinding defence);
- not carry an `Origin` other than `http://127.0.0.1:<port>`/`http://localhost:<port>`, nor `Sec-Fetch-Site: cross-site|same-site` (CSRF defence).

Otherwise it gets `403 {"error": ...}`.

| Route | Auth | Behaviour |
|---|---|---|
| `POST /api/v1/users/current/heartbeats(.bulk)`, same under `/api/hackatime/v1` | the Logbook API key, as wakatime-cli sends it (`Basic base64(key)`, also `Bearer`, `api_key=`) | Same request and response shapes as the server (`docs/api/ingest.md`): single → `201` (`/api/v1`) or `202` (`/api/hackatime/v1`); bulk → `201` with one `[body, status]` per item, `400` for invalid items. Valid heartbeats are queued and answered immediately; nothing waits on the network. `X-Machine-Name` and the `User-Agent` are folded into the heartbeat body (the server's attribution rule) so they survive the upload. If the database write fails the answer is `500`, so wakatime-cli keeps the heartbeats in its own offline queue. |
| `GET .../users/current/statusbar/today` | API key | Proxied to `<server>/api/v1/users/current/statusbar/today` with a 3 s timeout and a 30 s cache. If the server can't be reached, rejects the key or answers badly, the agent answers `200` with a WakaTime-shaped body computed from today's local heartbeat log (gaps up to 2 minutes, like the server) and the text suffixed with `(offline)` or `(re-login needed)`. |
| `POST /local/v1/events` | `X-Logbook-Token` = `cfg.LocalToken`, constant-time compare | JSON array of `wire.LocalEvent`, ≤ 1 MiB and ≤ 500 events. Invalid events are counted and skipped; valid ones are stored in one transaction. Answer: `200 {"accepted": n, "rejected": m}`. |
| `GET /` | none | Status page: server-rendered HTML, inline CSS, no scripts, `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'`. |
| `GET /local/v1/status` | none | `status.Snapshot` as JSON. |

The status page and JSON never contain the Logbook key, the Hackatime key or the local token.

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
| `201`, `202`, `409` | delete |
| other 4xx item, or a whole-batch 4xx other than 401/403/429 | delete, count in `upload.<name>.rejected`, log |
| `401` / `403` | keep, pause the worker and show "re-login needed" until the configured key changes |
| `429`, `5xx` (item or batch), network error | keep, back off exponentially from 5 s to 10 min with equal jitter; a longer `Retry-After` wins (capped at 30 min) |

Last success, last error and pause reason are saved in `kv` (`upload.<name>.*`), so `logbook status` shows them with the daemon stopped. On shutdown each worker makes one more attempt, bounded to 5 s, unless it is backing off.

## Daemon

`daemon.Run` binds the listener first; the port is the single-instance lock. If it's taken and an agent answers `/local/v1/status` there, `Run` returns `ErrAlreadyRunning` and `logbook run` exits 0. Then it opens the database and runs, under one `errgroup`: the API server, both workers, the spool drainer (at start and every minute), pruning (at start and every 6 h) and each sensor.

Sensors are supervised: a sensor that returns or panics is logged (with a truncated stack) and restarted after 1 s, doubling to 5 min; a run longer than a minute resets the backoff. Sensors receive the database as their `Sink`.

`config.json` is re-read when its modification time changes (checked at most every 5 s), so `logbook login` takes effect without a restart. A config that fails to load is ignored. A changed `local_addr` needs a restart.

### Hook spool

Hooks append to `spool/events.jsonl` when the daemon is down. The drainer:

1. renames it to `events.jsonl.<unix-nanos>.draining`, so new appends start a fresh file;
2. waits 2 s for any hook that opened the old file just before the rename;
3. reads it line by line (lines over 1 MiB and invalid events are skipped and counted), storing 500 events per transaction together with `kv` `spool.offset = <file>:<byte offset>`;
4. deletes it.

A crash mid-drain resumes from the saved offset, so events are neither lost nor duplicated. Leftover `.draining` files are processed first, oldest first. On Windows the rename fails while a hook holds the file open; the drainer retries a minute later.

### Active repos (`RepoSource`)

`daemon.NewRepos(db, nil)` lists directories seen in the last hour in heartbeats (the entity's directory) and AI events (`cwd`), most recent first (at most 200), walks each up to the nearest directory containing `.git` (directory or file, so worktrees and submodules count; at most 64 levels), and returns up to 50 repos. `Project` is the project name the heartbeat or AI event used, or else the repo directory's name. Lookups are cached for 10 minutes.

## Commands

| Command | Behaviour |
|---|---|
| `logbook run [--quiet] [--debug]` | Runs the daemon in the foreground. Logs (`log/slog` text) go to `agent.log` and, unless `--quiet`, best-effort to stderr. |
| `logbook login --key <key>\|- [--server <url>]` | Reads the key (from stdin with `-`, keeping it out of shell history), checks it with `GET /api/v1/users/current/statusbar/today`, and only then saves it, creating the config with `config.Default()` if needed. Other config fields are kept. Never prints the key. |
| `logbook status [--json]` | Asks the daemon; if it's down, reads the database directly. |
| `logbook doctor [--json]` | Checks config, key validity (server), clock skew against the server's `Date` header (warns above 2 min), whether the daemon answers, whether `~/.wakatime.cfg` (or `$WAKATIME_HOME/.wakatime.cfg`) has `api_url` pointing at the agent's port and the same `api_key`, queue health (re-login needed, dropped rows, items older than 1 h) and the spool size. Prints a fix for every problem and exits 1 if any check fails. |

## OS-specific notes

The core has no OS-specific code paths and builds with `CGO_ENABLED=0` for windows, darwin and linux on amd64 and arm64. Behaviour that differs by OS:

- **Windows:** file modes are advisory; `config.Dir()` is `%AppData%\logbook`. `os.Rename` replaces existing files (log rotation relies on this). Renaming the spool fails while a hook holds it open (see above). Stderr may be unusable when started from the `Run` key, so logging to it is best effort.
- **macOS:** `config.Dir()` is `~/Library/Application Support/logbook`.
- **Linux:** `config.Dir()` is `$XDG_CONFIG_HOME/logbook` (default `~/.config/logbook`).

## Performance

Idle, the daemon holds one listener, two 10 s timers, a 1-minute spool timer and a 6-hour prune timer. Measured on linux/amd64 with no sensors: about 14 MB RSS and no measurable CPU. Heartbeat writes are batched per request; spool drains 500 events per transaction.
