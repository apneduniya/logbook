# 0009: Local agent design

- Status: accepted
- Date: 2026-11-20

## Context

The agent (`cmd/logbook`) runs on teenagers' laptops, often 10-year-old Windows machines. It has to:
- pass editor heartbeats on to Logbook and mirror them to Hackatime (ADR 0005);
- sense activity (ADR 0007);
- record AI harness events and git activity;
- keep evidence locally (ADR 0006).

It must install without admin rights, never slow down editors or AI tools, and cross-compile from Linux.

## Decision

**Binary.** A single static Go binary (`CGO_ENABLED=0`) per OS and architecture. OS APIs are called without cgo:
- Windows: `golang.org/x/sys/windows` with lazy DLLs;
- macOS: `github.com/ebitengine/purego` for CoreGraphics and AppKit;
- Linux: D-Bus via `github.com/godbus/dbus/v5`, and X11 via a pure-Go client.

**Files.**
- Data lives in `os.UserConfigDir()/logbook/`:
  - `config.json`, mode 0600;
  - `logbook.db`, a SQLite database using `modernc.org/sqlite` with WAL mode;
  - `agent.log`, rotated.
- The local database holds:
  - the upload and mirror queues;
  - activity samples, AI events and git events (raw, never uploaded in step 2);
  - settings.

**Process.**
- `logbook run` is the long-running daemon. It's started at login by an autostart entry the installer writes:
  - Windows: an `HKCU\...\Run` registry value;
  - macOS: a LaunchAgent plist;
  - Linux: a systemd `--user` unit, falling back to XDG autostart.
- No tray icon in V1: on macOS and Linux it would need cgo. The daemon serves a local status page instead.

**Local HTTP server** on `127.0.0.1:47231` (configurable).
- WakaTime-compatible endpoints: `/api/v1/users/current/heartbeats(.bulk)` and `/statusbar/today`.
  - The installer points `api_url` in `~/.wakatime.cfg` here and sets `api_key` to the user's Logbook key.
  - The user's original Hackatime key and URL are saved in the agent config, for mirroring.
  - The original `.wakatime.cfg` is backed up and restored on uninstall.
- `POST /local/v1/events` takes AI, activity and git events from hook commands. It requires the random local token from `config.json` in the `X-Logbook-Token` header.
- `GET /` serves the status page.
- Every request's `Host` header must be `127.0.0.1:<port>` or `localhost:<port>`, which blocks DNS rebinding. Browser requests need the token, which blocks CSRF from web pages.

**Uploads.**
- Heartbeats are queued in SQLite and uploaded to the Logbook server in batches of up to 100 every ~10 s.
- A separate queue mirrors heartbeats to Hackatime, unless the user turns mirroring off.
- Both queues back off exponentially with jitter and survive restarts.
- A 4xx response other than 429 drops the item and logs it; 5xx responses and network errors retry.

**Hook commands.** `logbook hook <harness> <event>` is what AI harnesses run on their hooks. It:
- reads the harness's JSON from stdin;
- extracts **metadata only**: event type, time, session id, cwd → project, tool name, model, and prompt *length* (never prompt text, file contents or tool output);
- posts it to the local server with a 300 ms timeout;
- always exits 0 quickly, so the harness is never blocked or broken. If the daemon is down, the event is appended to a spool file that the daemon picks up later.

**Sensors** run inside the daemon:
- idle seconds and the focused app, every 5 s (ADR 0007);
- git: for active projects, new commits (hash, time, files changed, insertions and deletions, author is the user) and the working tree's diff size, every 60 s, by running the system `git` with a timeout.

**Installing.**
- `logbook install` does everything idempotently: writes config, rewrites `.wakatime.cfg`, installs hooks into the detected harnesses, sets up autostart and starts the daemon.
- `logbook uninstall` reverses all of it.
- `logbook login --key <api-key>` (device pairing comes later with the web UI).
- `logbook status` and `logbook doctor` help with troubleshooting.

**Shared types.** The types the daemon and hook commands exchange (`wire.AIEvent`, `wire.ActivitySample`, `wire.GitEvent`) live in `pkg/wire`. Step 4 uploads them inside evidence bundles.

## Consequences

- No cgo means simple cross-compiling, and no compiler is needed on users' machines.
- On Wayland, the focused app is often unavailable; the agent records it as `unknown` and relies on idle time plus heartbeats.
- If someone kills the daemon, editor heartbeats wait in wakatime-cli's offline queue and hook events in the spool file, so nothing is lost.
- CI must build all OS targets and run the OS-specific tests on Windows and macOS runners.
