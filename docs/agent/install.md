# Agent install and uninstall

Owned by `internal/agent/install` and its subpackages (`wakacfg`, `autostart`, `selfinstall`, `keycheck`, `atomicfile`), the `install` and `uninstall` commands in `cmd/logbook/cmd_install.go`, and the one-line installers in `scripts/`. Design: ADR 0009 and ADR 0005.

Nothing here needs admin or root rights. Everything goes in the user's home directory or `HKCU`.

## One-line installers

| OS | Command |
|---|---|
| macOS, Linux | `curl -fsSL https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.sh \| LOGBOOK_API_KEY=<key> sh` |
| Windows | `$env:LOGBOOK_API_KEY="<key>"; irm https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.ps1 \| iex` |

Both scripts:
1. detect the OS and CPU (`amd64` or `arm64`; an Intel shell under Rosetta gets the native `arm64` build; 32-bit PowerShell on 64-bit Windows gets the 64-bit build);
2. download `logbook_<os>_<arch>[.exe]` and `checksums.txt` from the GitHub release (`LOGBOOK_VERSION=<tag>` pins one; the default is the latest);
3. check the binary's SHA-256 against `checksums.txt` and stop on any mismatch or missing entry;
4. run `<binary> install` with any extra arguments.

`install.sh` is POSIX `sh` (tested with dash and busybox) and refuses to run as root. `install.ps1` works on Windows PowerShell 5.1, turns on TLS 1.2 for old Windows 10 builds, and never calls `exit`, so `irm | iex` can't close the user's window. Passing the key in `LOGBOOK_API_KEY` rather than `--key` keeps it out of the process list.

## `logbook install`

```
logbook install --key <key> [--server <url>] [--no-mirror] [--mirror-url <url>] [--harness all|none|claude-code,codex,...]
```

`--key` falls back to `LOGBOOK_API_KEY`. Install refuses to run in a build without the `run` daemon command, because every autostart entry runs it (and `release.yml` refuses to publish such a build). Input is checked before anything changes: the key must be a Logbook key (lowercase UUIDv4), `--harness` names must exist, URLs must be http(s). The steps then run in this order, each printing one line:

| # | Step | What changes | Undo on a later failure |
|---|---|---|---|
| 1 | config | Loads `config.json` or creates it with `config.Default()` (new local token). Checks the key with `GET <server>/api/v1/users/current/statusbar/today` (`Authorization: Basic base64(key)`, 15 s timeout; https only, except plain http to localhost). Saves the key and server. | Restore the previous config, or delete a new one. |
| 2 | binary | Copies the running binary into place (below). | Put the previous binary back, or delete the new one. |
| 3 | wakatime | Points `.wakatime.cfg` at the daemon (below) and saves the mirror target in `config.json`. | First install: restore the file as uninstall would. Re-install: put back the file bytes and state this run replaced (for example the old `api_key` after a new `--key`), so the file and `config.json` agree. |
| 4 | hooks | For each selected harness that `Detect()`s, `Install(<installed binary>)`. A harness that fails is reported and skipped; it doesn't undo the install. | `Uninstall()` hooks this run added. |
| 5 | autostart | Writes the login entry (below), then `Start()`s the daemon, restarting one that is already running so the new binary takes over. | Remove an entry this run added, including one left half-done when `Install` itself failed (unit written but not enabled, plist written but not loaded). |

Then the previous binary is deleted, and a summary prints the status page URL. On macOS and Linux it also says how to put the binary's folder on `PATH` when it isn't there; on Windows, where the folder is never added to `PATH`, it prints the full path of `logbook.exe` and the PowerShell command to uninstall (`& "<path>" uninstall`). Running install again with the same flags changes nothing; a failed re-install undoes only its own changes and leaves the working install alone.

Harness installers come from `harness.All()` (the harness module). Until that is merged, `harnessInstallers()` returns an empty list.

## `.wakatime.cfg` (`wakacfg`)

**Which file.** The same one wakatime-cli reads (`pkg/ini.FilePath`, USAGE.md "Config File"):
1. `$WAKATIME_HOME/.wakatime.cfg` when `WAKATIME_HOME` is set (a leading `~` is expanded);
2. `~/.wakatime.cfg` if it exists;
3. `$XDG_CONFIG_HOME/wakatime/wakatime.cfg` (default `~/.config/wakatime/wakatime.cfg`) if it exists;
4. otherwise a new `~/.wakatime.cfg`. wakatime-cli would pick the XDG path here, but editor plugins read and create `~/.wakatime.cfg` directly and wakatime-cli prefers it once it exists, so the legacy path is the one every reader agrees on.

A symlinked file (dotfiles repos) is edited through the link, which stays a link. The file's permissions are kept; a new file is `0600`.

**What changes.** Only `api_url` and `api_key` in `[settings]`:
- `api_url = http://<local_addr>/api/v1` (default `http://127.0.0.1:47231/api/v1`)
- `api_key = <Logbook key>`

Every other byte stays: comments, blank lines, other keys and sections, key order, a UTF-8 BOM, CRLF line endings, a missing final newline. Section and key names match case-insensitively (viper lowercases them). If a key appears more than once, every copy is updated. Indented continuation lines of an old value are removed with it. Missing keys go after the last key of `[settings]`, and a missing `[settings]` is added at the end.

**Refused.** If `import_cfg` names a file that sets `api_url` or `api_key`, install stops with an error: wakatime-cli reads that file last, so it would override Logbook.

**Warned.** `[api_urls]` (projects sent to other servers), `[project_api_key]` (other keys for some projects) and `api_key_vault_cmd` are left alone and reported, because heartbeats they affect may bypass Logbook or can't be mirrored.

**Files in the data directory** (both `0600`):
- `wakatime.cfg.backup`: the file exactly as it was before the first install. It is never overwritten. If one is already there from an older install that was undone by hand, it is kept, and uninstall only restores keys (below).
- `wakatime.cfg.state.json`: which file was changed, whether it existed, the SHA-256 of what install wrote, the local URL, and the original `api_url`/`api_key` (with whether each was present). It is written before the config file, so a crash in between can still be undone.

**Original key.** wakatime-cli reads `api_key`, then the legacy `apikey`, then `WAKATIME_API_KEY`. The first non-empty one is saved as the key to mirror with. Only `api_key` is ever written; a legacy `apikey` line stays in the file untouched (`api_key` wins while Logbook is installed), and uninstall removes the `api_key` line Logbook added.

**Re-install.** If the file changed since Logbook last wrote it (the user or a plugin edited it), a re-install marks the backup as no longer exact, so uninstall later restores only the keys and keeps those edits.

**Uninstall:**
- File unchanged since install: the backup is copied back byte for byte (or the file is deleted, if install created it), and the backup is removed.
- File edited since (by the user or a plugin) but still pointing at Logbook: only `api_url` and `api_key` go back to their originals (or are removed, if there were none). The other edits stay, and the backup is kept and its path printed.
- File no longer points at Logbook: left alone.
- The state file is always removed.

## Mirroring

The mirror target is whatever `.wakatime.cfg` pointed at before, so nothing the user relies on breaks:

| Original file | Mirror |
|---|---|
| Hackatime (`hackatime.hackclub.com` or `/api/hackatime/`) | to Hackatime |
| WakaTime, or no `api_url` (wakatime-cli's default `https://api.wakatime.com/api/v1`) | to WakaTime |
| Any other server (e.g. self-hosted wakapi) | to that server |
| Already the Logbook server or the local daemon | off (it would count time twice) |
| No `api_key` (the legacy `apikey`, then the `WAKATIME_API_KEY` env var, count as a key, in wakatime-cli's order) | off |

`--no-mirror` saves the URL and key with `mirror: false`. `--mirror-url` keeps the original key but sends to a different URL. A re-install without either flag keeps the earlier choice, so to turn mirroring back on after `--no-mirror`, re-install with `--mirror-url <saved url>`; the install output prints that exact command. The original key is stored only in `config.json` and the state file (both `0600`, in the `0700` data directory) and is never printed.

## Binary location (`selfinstall`)

| OS | Path |
|---|---|
| Windows | `%LOCALAPPDATA%\Programs\Logbook\logbook.exe` (`%USERPROFILE%\AppData\Local` if unset) |
| macOS | `~/Library/Application Support/Logbook/bin/logbook` |
| Linux | `~/.local/bin/logbook` (a hint is printed when it is not on `PATH`) |

The binary is copied to a temp file in the same directory, synced, then renamed into place (mode `0755`). An identical binary, or the installed binary installing itself, is left alone. An existing binary is first renamed to `logbook.old-<id>`: Windows can rename a running `.exe` but not overwrite or delete it. Leftover `.old-*` files are deleted on the next install. On uninstall, Windows can't delete the running binary, so a hidden `cmd.exe` retries `del` for about 30 seconds after the process exits and then removes the empty `Logbook` folder. No admin rights are needed, unlike `MoveFileEx(MOVEFILE_DELAY_UNTIL_REBOOT)`. The `Logbook` directories on Windows and macOS are removed when empty; `~/.local/bin` is shared and stays.

## Autostart (`autostart`)

Every entry runs `<installed binary> run`. The file contents are built by pure functions with golden tests (`autostart/testdata/`).

**Windows.**
- Value `Logbook` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, set to `"<path>" run` (REG_SZ, quoted because the path may contain spaces).
- `Start` first runs `taskkill /F /IM logbook.exe /FI "PID ne <self>"`, so an older daemon holding the port goes away. It then starts the daemon with `CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP`, so the daemon has a hidden console that `git` and other console children inherit, and no window flashes. Its working directory is the binary's folder.
- At login, Windows starts the Run value as a console program, so Explorer gives it its own visible console (or a Windows Terminal window on Windows 11), and closing that window would kill the daemon. `autostart.Background(bin, args)` handles this and **must be the first thing `logbook run` does on Windows**: when the process is the only one attached to its console (`GetConsoleProcessList` returns 1) and `LOGBOOK_DETACHED` isn't `1`, it starts a hidden copy (same arguments, `CREATE_NO_WINDOW`, `LOGBOOK_DETACHED=1`) and returns true, and `run` exits. The window flashes for a moment at login, then closes. A daemon started from a terminal shares that console with the shell and stays in the foreground. `Start` sets `LOGBOOK_DETACHED=1` too. On macOS and Linux `Background` does nothing.
- Uninstall deletes the value and runs the same `taskkill`.

**macOS.**
- `~/Library/LaunchAgents/dev.boogles.logbook.plist`, with:
  - `RunAtLoad`;
  - `KeepAlive` = `{SuccessfulExit: false}`, so it restarts only after a crash or non-zero exit;
  - `ThrottleInterval` 30;
  - `ProcessType Background`, which gives lower CPU and I/O priority;
  - stdout and stderr appended to `<data dir>/launchd.log`.
- Install runs `launchctl bootstrap gui/<uid> <plist>`. If the job is already loaded (`launchctl print` succeeds) and the plist changed, it runs `launchctl bootout gui/<uid>/dev.boogles.logbook` first, then polls `launchctl print` every 250 ms (up to 5 s) until launchd has really dropped the job: `bootout` returns before that, and an early `bootstrap` fails with `Bootstrap failed: 5: Input/output error`. An unchanged, loaded job is left alone.
- `Start` runs `launchctl kickstart -k gui/<uid>/dev.boogles.logbook`.
- Uninstall runs `bootout` and deletes the plist.

**Linux.**
- If `systemctl --user show-environment` works, the agent writes `$XDG_CONFIG_HOME/systemd/user/logbook.service` (default `~/.config/...`) with:
  - `Restart=on-failure`, `RestartSec=10`;
  - `MemoryMax=64M`, `CPUQuota=10%`;
  - `Nice=10`, best-effort I/O priority 7;
  - on a desktop (`systemctl --user is-active graphical-session.target` succeeds at install): `PartOf=`, `After=` and `WantedBy=graphical-session.target`. Desktops import `DISPLAY`, `WAYLAND_DISPLAY` and the session bus address into the user manager before they start that target, so the X11/Wayland idle and focus sensors see them, and the daemon stops with the session. `After=` alone would not help a unit pulled in by `default.target`, which the user manager reaches at first login, before the desktop starts;
  - otherwise (install over SSH, WSL, desktops without systemd integration): `WantedBy=default.target`. Re-running install from the desktop switches the unit over.

  It then runs `systemctl --user daemon-reload` and `reenable logbook.service` (disable then enable, so a link under the other target from an earlier install is dropped). `Start` runs `systemctl --user restart logbook.service`. Uninstall runs `disable --now`, deletes the unit and runs `daemon-reload`.
- Without a user systemd (WSL, minimal distros), it writes `$XDG_CONFIG_HOME/autostart/logbook.desktop` (`Type=Application`, `NoDisplay=true`, `Terminal=false`, `Exec` quoted per the Desktop Entry spec). `Start` runs `pkill -f '(^|/)logbook run$'` and then starts the daemon in a new session (`setsid`). Uninstall stops it the same way and deletes the file.
- Installing one kind removes the other, so two daemons never run.

Every `launchctl`, `systemctl`, `pkill` and `taskkill` call has a 15 s timeout.

## `logbook uninstall [--purge --yes]`

Steps run in reverse order, and a failed step doesn't stop the rest:
1. Remove autostart, which also stops the daemon.
2. Remove the hooks of every harness whose `Installed()` is true.
3. Restore `.wakatime.cfg`.
4. Remove the binary.

The data directory (config, local database, logs) is kept unless `--purge --yes` is given. `--purge` without `--yes` fails before anything changes. Purge is skipped if any earlier step failed, because the directory holds what a retry needs. If the `.wakatime.cfg` backup wasn't used, it is first copied to `<.wakatime.cfg path>.logbook-backup`, so the user's original is never lost. Purge refuses a filesystem root and the home directory. It deletes only a directory that holds `config.json`, `wakatime.cfg.state.json` or `wakatime.cfg.backup`, or that is exactly the default data directory (`<user config dir>/logbook`). A directory that is merely named `logbook`, such as a checkout a mistaken `LOGBOOK_HOME` points at, is refused. On Windows it retries for a couple of seconds while the stopped daemon releases its files.

## Releases

`.github/workflows/release.yml` runs on `v*` tags. It runs the agent tests, then builds `logbook_{linux,darwin,windows}_{amd64,arm64}[.exe]` with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<tag>"`, writes `checksums.txt` (`sha256sum` format), and publishes them with `gh release create --verify-tag --generate-notes`. The Go linker ad-hoc signs darwin/arm64 binaries, which Apple silicon requires.
