# Agent sensors

The sensors run inside the daemon (`logbook run`) and record activity and git events through the daemon's `Sink` (ADR 0007, ADR 0009). They write no files of their own, start no long-lived processes and never send anything off the device; the daemon decides what is stored.

| Package | Job |
|---|---|
| `internal/agent/sensors/activity` | The activity sensor: idle time + focused app, every 5 s |
| `internal/agent/sensors/osprobe` | Per-OS calls for idle time, focused app and screen lock |
| `internal/agent/sensors/focusclass` | Classifies an app name into editor / terminal / browser / ai / other / unknown |
| `internal/agent/sensors/gitwatch` | The git sensor: your commits and the uncommitted diff size, every 60 s |

## What is never recorded

Window titles, keystrokes, clipboard, screen contents, file contents, commit messages, author names, file paths in diffs, and the command lines of processes. The focused app is a process name (Windows, Linux) or bundle identifier (macOS) only.

## Activity sensor

Every 5 s it reads, in order: screen lock, idle time, then (only when unlocked) the focused app. It records:

```
wire.ActivitySample{Time: now (UTC), IdleSeconds: idle (0.1 s precision), FocusApp: name or "", FocusKind: kind}
```

- `FocusKind` is `unknown` whenever `FocusApp` is empty (Wayland, lock screen, protected process).
- **Idle unknown:** if no OS idle source works, the sensor records **no sample** and polls every 30 s. Recording `IdleSeconds: 0` would claim the user is active. The daemon treats a gap in samples as "activity unknown" and falls back to heartbeat recency.
- **Cadence:** 5 s normally; 30 s while the screen is locked, while idle time is unknown, or once idle time reaches 10 minutes. It returns to 5 s on the first sample after input resumes (so at most 30 s late).
- Each sample costs one to three OS calls on reused connections. No process is spawned per sample.
- Errors never stop the sensor. Each failing call (lock, idle, focus, sink) is logged once when it starts failing and once when it recovers.

### Windows

- Idle: `user32!GetLastInputInfo` (session-wide last input, as a 32-bit tick count) against `kernel32!GetTickCount64`. The subtraction is done in 32-bit arithmetic, so it stays correct when the 32-bit counter wraps every 49.7 days (`idleFromTicks`, unit-tested).
- Focus: `GetForegroundWindow` → `GetWindowThreadProcessId` → `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` → `QueryFullProcessImageNameW` → base name, e.g. `Code.exe`. No admin rights needed. Elevated or protected processes that refuse even a limited query are reported as unknown.
- Lock: `OpenInputDesktop`. When the lock screen (or a UAC prompt) is up, the input desktop is the secure `Winlogon` desktop, which a normal process can't open, or one whose name isn't `Default`.
- DLLs load with `NewLazySystemDLL`, so only from System32 (no DLL planting).
- `probe_windows_test.go` runs the real probe on the Windows CI runner: every call returns promptly with sane values or an error.

### macOS

- Uses `github.com/ebitengine/purego` to `dlopen` the frameworks. purego's internal `fakecgo` lets this work with `CGO_ENABLED=0` on darwin/amd64 and darwin/arm64 (Tier 1 in purego's README; no special flags).
- Idle: `CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateCombinedSessionState, kCGAnyInputEventType)`. **No permission prompt**: it reads a counter, not events, so neither Input Monitoring nor Accessibility is needed.
- Focus: `CGWindowListCopyWindowInfo(kCGWindowListOptionOnScreenOnly | ExcludeDesktopElements)` gives windows front to back. The first window on layer 0 is the frontmost app's. Its owner PID goes to `NSRunningApplication runningApplicationWithProcessIdentifier:`, and the sensor records `bundleIdentifier` (falling back to `localizedName`, then the window owner name). **No permission prompt**: window owner PIDs and names don't need Screen Recording permission. Only window titles (`kCGWindowName`) do, and they're never read.
  - Why not `NSWorkspace.sharedWorkspace.frontmostApplication`: NSWorkspace refreshes it from notifications delivered on the main run loop, which a daemon without an `NSApplication` run loop never runs, so it goes stale.
  - The Objective-C calls run inside an `NSAutoreleasePool` on a locked OS thread, so autoreleased strings don't leak.
- Lock: `CGSessionCopyCurrentDictionary()["CGSSessionScreenIsLocked"]`.
- If a framework fails to load, every call returns that error once logged, and the sensor records nothing (idle unknown).
- `probe_darwin_test.go` runs the real probe on the macOS CI runner: the frameworks must load through purego with `CGO_ENABLED=0`, and every call must return promptly without crashing. A code review ran the CGO_ENABLED=0 build on macOS arm64, and it read idle time, the bundle id `com.brave.Browser` (classified as browser) and the lock state.
- **Still to confirm on a real Mac:** that a LaunchAgent (Aqua session) gets a non-nil session dictionary.

### Linux

Idle sources are tried in a fixed order. The first that answers is remembered, so steady-state sampling is a single D-Bus call (or, for Wayland, no call at all):

1. `org.gnome.Mutter.IdleMonitor.GetIdletime` on the session bus (GNOME on X11 and Wayland).
2. `org.freedesktop.ScreenSaver.GetSessionIdleTime` on the session bus. KDE Plasma answers this **on X11 only**: on Wayland, kscreenlocker replies `NotSupported`, so Plasma Wayland falls through to 3.
3. Wayland `ext-idle-notify-v1` (KWin, Sway, Hyprland, niri, labwc and other wlroots compositors). Not tried on X11 sessions. See below.
4. The X11 `MIT-SCREEN-SAVER` extension (`MsSinceUserInput`) via `github.com/jezek/xgb`. **Only on real X11 sessions**: under Wayland, XWayland only sees input sent to X11 windows.
5. logind `IdleHint` / `IdleSinceHint` for the user's display session (system bus), only when that session's `Type` is `x11`, `wayland` or `mir`. Without a desktop, logind elects a text session as the display session, even a remote SSH login, and its hint then tracks a terminal the person may not be using. The type is checked on every read, because it can change while the session lives (a compositor started from a tty and then quit leaves a `tty` session). A session that isn't a desktop is ignored and looked up again on the next try. **Only trusted once IdleHint has been seen true in this session.** Several desktops never set it: Plasma (KDE bug 448942) and wlroots compositors without an idle daemon calling `SetIdleHint`. There IdleHint stays false forever, which would read as "always active", so until the desktop is seen setting it, logind reports unavailable and the sensor records nothing. Even when trusted it is coarse: `IdleSinceHint` is when the desktop flipped the hint, which is the desktop's idle delay (often 5 minutes) after the last input. Idle time is understated by that delay, and shorter idle periods read as 0.

Chain behaviour:

- If the remembered source fails, the chain falls through to the next one. A **timeout** doesn't count as failure until it happens 3 samples in a row: one slow reply from a busy compositor costs that sample (no sample is recorded), not a 5 minute switch to a worse source.
- Every 5 minutes the chain re-tries the better sources (for example Mutter starting after the daemon at login). If none works, it logs one warning and re-scans every 5 minutes.

**ext-idle-notify-v1.** A small built-in client speaks the Wayland wire protocol over `$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY` (defaults: `/run/user/<uid>`, `wayland-0`). No library, no file descriptors, no surfaces or windows. It binds `wl_seat` and `ext_idle_notifier_v1` and creates one notification with a 5 s timeout. With notifier version 2 it uses `get_input_idle_notification`, which ignores idle inhibitors such as a playing video; with version 1 inhibitors count as activity. A goroutine reads the compositor's `idled`/`resumed` events, so a sample costs no round trip.

- Idle time is `now − idled + 5 s` while idle, and 0 within 5 s of input (5 s precision).
- The protocol counts from when the notification is created, so right after the daemon connects, idle reads 0 for up to 5 s even if the user was already away.
- The handshake has a 1 s deadline. If the connection drops, the source fails over and reconnects at most once a minute.

**Session type** comes from `XDG_SESSION_TYPE`, `WAYLAND_DISPLAY` and `DISPLAY`. A `systemd --user` unit often starts before the desktop imports these into the user manager, and a running process never sees later imports. So when they are missing, the probe asks logind every 5 minutes for the display session's `Type` (`x11`/`wayland`) and `Display` (`:0`), and switches on X11 focus and idle once it knows. X11 still needs a readable Xauthority: `$XAUTHORITY`, else `~/.Xauthority`. Display managers that keep the cookie elsewhere (GDM on X11: `/run/user/<uid>/gdm/Xauthority`) need `XAUTHORITY` in the unit's environment.

**Connections.** Every connect is bounded by a 1 s timeout, so a wedged bus daemon or X server (for example a stale forwarded `DISPLAY`) never blocks the sensor:

- D-Bus: dial, auth and `Hello`. The session bus is found from `DBUS_SESSION_BUS_ADDRESS` or `/run/user/<uid>/bus` and is **never autolaunched**: the library's `ConnectSessionBus` would run `dbus-launch`, and the sensor doesn't.
- X11: the setup handshake.
- If a connect outlives its timeout, later samples wait on the same attempt instead of starting another, so at most one goroutine per connection is ever stuck.
- D-Bus calls use `NoAutoStart`, so probing never launches a service, and have a 1 s timeout each. Connections are opened lazily, reused, and reopened after an error.

**Focus (X11 only):** root `_NET_ACTIVE_WINDOW`, then the PID of the X client that owns that window, then `/proc/<pid>/comm` (the kernel process name, at most 15 bytes; never `cmdline`).

- The PID comes from the X server's **X-Resource** extension (`XResQueryClientIds` with `LocalClientPID`). The server learns it from the socket, in the host's PID namespace, so Flatpak and other sandboxed apps resolve to their real host process.
- `_NET_WM_PID` isn't trusted. Inside a Flatpak PID namespace it is the in-sandbox PID, often 2, which is `kthreadd` on the host. For an `ssh -X` client it is a PID on another machine.
- Remote clients have no local PID, so they are reported as unknown.
- Only if the server lacks X-Resource 1.2 (older than Xorg 1.14, 2013) does the probe fall back to `_NET_WM_PID`, and then only when the window's `WM_CLIENT_MACHINE` is this host. A Flatpak app can still be misnamed in that fallback.
- Window managers without EWMH report unknown.

**Other Linux details:**

- Focus on **Wayland is unknown** by design. There's no portable, permission-free API, and GNOME Shell `Eval` is not used.
- Lock: logind `LockedHint` on the display session. Desktops that don't set it read as unlocked; the idle threshold still triggers the 30 s cadence there.
- Every X11 request has a 1 s timeout. xgb requests can't be cancelled, so on a timeout the connection is closed, which fails the request, and it's reopened at most once a minute.

## Focus classification

`focusclass` is a data table in `rules.go`, matched case-insensitively and per OS. Windows drops `.exe`, and Linux also matches names cut to 15 bytes by `/proc/<pid>/comm` (`gnome-terminal-`). Generic names only apply where they mean a terminal (`cmd` on Windows, `st` on Linux). It covers:

- editors: the VS Code family (Code, Insiders, VSCodium, Cursor, Windsurf, Trae, Kiro, Positron), JetBrains IDEs including Android Studio and Fleet, Zed, Sublime, Vim/Neovim GUIs, Emacs, Xcode, Visual Studio, Arduino IDE, KiCad, Godot, Unity, Thonny, BlueJ and more;
- terminals: Windows Terminal, cmd, PowerShell, mintty, iTerm2, Terminal.app, Ghostty, Alacritty, kitty, WezTerm, Warp, GNOME Terminal/Console/Ptyxis, Konsole, foot and more;
- browsers: Chrome, Edge, Firefox, Safari, Brave, Arc, Dia, Opera, Vivaldi, Zen, Comet and more;
- AI apps: Claude (`com.anthropic.claudefordesktop`), ChatGPT (`com.openai.chat`, and the unified app `com.openai.codex`), Codex, Perplexity, LM Studio.

Anything else is `other`, and an empty name is `unknown`. Cursor's macOS bundle id is the generated `com.todesktop.230313mzl4w4u92`.

## Git sensor

Every 60 s it asks the daemon's `RepoSource` for repos with activity in the last hour, and for each absolute repo path runs the system `git`:

| Call | Purpose |
|---|---|
| `git rev-parse -q --verify HEAD^{commit}` | skip unborn branches (exit 1) quietly |
| `git config -z --get-regexp '^(user\.email$\|filter\.)'` | the repo's effective `user.email` (unset → commits skipped, logged once) and the names of configured filter drivers (their commands are discarded unread) |
| `git log HEAD --format=%x1e%H%x1f%at%x1f%ae%x1f%P --shortstat --since=<now-1h> --max-count=200` | your commits: hash, author time, parents, files, insertions, deletions |
| `git rev-parse --git-path shallow` | only when a parentless commit shows up: where the shallow-boundary list is |
| `git -c filter.<name>.clean= … diff --shortstat HEAD --` | uncommitted diff size (tracked files; untracked files aren't counted), with every filter driver turned off |

**Which commits are recorded:**

- Only commits whose author email equals `user.email` (case-insensitive). Commits by others, such as pulled or merged work, are ignored.
- **Only commits authored in the last hour.** `--since` filters on committer time, which a rebase, cherry-pick or amend refreshes. The author time check keeps a rebased week-old branch from being recorded as new work.
- **Rewrites aren't recorded twice.** Amend, rebase and cherry-pick keep the author time and make a new hash. A new hash whose author time equals an already-handled commit that is no longer on HEAD is treated as that commit rewritten: it is remembered, not recorded, and the old entry can't match again. The recorded stats stay those of the first version. Two real commits in the same second both stay on HEAD, so both are recorded.
- Limit: a new commit authored in the same second as a commit that was just rewritten away is missed.
- **Shallow-clone boundaries aren't recorded.** In a shallow clone the boundary commit has no parents, so git diffs it against the empty tree and reports the whole repository as insertions. A parentless commit listed in `.git/shallow` came from a fetch, not from work in this clone, so it is skipped. A true root commit (a new repo's first commit) is recorded normally.

**Events:**

- `commit`, with Time = author time, for each new commit. `worktree`, with Time = now, when the diff size differs from the last recorded snapshot; the first poll of a repo always emits one.
- **Duplicates:** handled commits live in memory only. A new process, or a repo seen for the first time, re-emits its commits from the last hour, so **the daemon must dedup commit events by (project, hash)**.
  - A commit amended while the daemon was stopped has a new hash after restart, so the daemon's hash dedup counts both versions. The window is one hour.
  - Entries are forgotten once their author time is 2 h old, and at most 400 are kept per repo.
- **Sink failures are retried.** State for an event (its hash handled, the worktree snapshot) is updated only after the sink accepts it. A rejected event, such as a locked database, is offered again on the next poll. A failed git call changes no state, so that poll is retried in full.
- **Every git process:** `exec.CommandContext` with a 5 s timeout and `WaitDelay` of 1 s; stdout capped at 1 MiB; empty stdin.
  - Environment: `GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`; inherited `GIT_DIR`, `GIT_WORK_TREE`, `GIT_EXTERNAL_DIFF`, `GIT_SSH*`, askpass and `GIT_CONFIG_PARAMETERS` are removed.
  - Flags: `--no-pager -c core.fsmonitor=false -c core.untrackedCache=false -c log.showSignature=false`, plus `--no-ext-diff --no-textconv --no-renames`.
  - **Filter drivers are turned off.** `git diff` runs a repo's clean/process filters (`filter.<name>.clean` in `.git/config` plus `.gitattributes`) on every stat-dirty tracked file. A project folder copied with its `.git` (zip, USB stick, shared drive) brings its config along. So the sensor lists the configured filter names and passes `-c filter.<name>.clean= -c filter.<name>.smudge= -c filter.<name>.process= -c filter.<name>.required=false` for each. Empty commands are no-ops to git, and this is tested with a real repo whose clean filter would leave a marker file. A filter name that can't be passed back on the command line (it contains `=`) makes the sensor skip that repo's diff rather than run it.
  - With that, nothing in a repo's config makes the sensor run another program: no fsmonitor hook, external diff, textconv, filter driver or gpg.
  - Side effects: with `GIT_OPTIONAL_LOCKS=0` the index is never refreshed, so git re-hashes a stat-dirty tracked file on each poll until the user's own git refreshes it. The sensor only gets the counts. A Git LFS file that is stat-dirty but unchanged is compared raw against its pointer and counts as one changed file until then.
- **Failures** (timeout on a huge repo, "dubious ownership", not a repo) skip that repo for 10 minutes and log once per failure streak.
- **Errors** never contain config values or command output beyond git's first stderr line; `-c` options are skipped when naming the failing subcommand.
- **Git missing:** looked up once with `exec.LookPath`. If absent, the sensor logs one warning and idles until shutdown.
- **Bounds:** at most 256 repos tracked; repos inactive for 2 h are forgotten; at most 200 commits per poll.
- Detached HEAD and shallow clones work unchanged (both are tested). Windows paths are passed as `cmd.Dir`, never as arguments.
- **Windows:** git runs with `CREATE_NO_WINDOW`, so no console window flashes every minute.
