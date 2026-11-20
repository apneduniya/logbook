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

### macOS

- Uses `github.com/ebitengine/purego` to `dlopen` the frameworks. purego's internal `fakecgo` lets this work with `CGO_ENABLED=0` on darwin/amd64 and darwin/arm64 (Tier 1 in purego's README; no special flags).
- Idle: `CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateCombinedSessionState, kCGAnyInputEventType)`. **No permission prompt**: it reads a counter, not events, so neither Input Monitoring nor Accessibility is needed.
- Focus: `CGWindowListCopyWindowInfo(kCGWindowListOptionOnScreenOnly | ExcludeDesktopElements)` gives windows front to back. The first window on layer 0 is the frontmost app's. Its owner PID goes to `NSRunningApplication runningApplicationWithProcessIdentifier:`, and the sensor records `bundleIdentifier` (falling back to `localizedName`, then the window owner name). **No permission prompt**: window owner PIDs and names don't need Screen Recording permission. Only window titles (`kCGWindowName`) do, and they're never read.
  - Why not `NSWorkspace.sharedWorkspace.frontmostApplication`: NSWorkspace refreshes it from notifications delivered on the main run loop, which a daemon without an `NSApplication` run loop never runs, so it goes stale.
  - The Objective-C calls run inside an `NSAutoreleasePool` on a locked OS thread, so autoreleased strings don't leak.
- Lock: `CGSessionCopyCurrentDictionary()["CGSSessionScreenIsLocked"]`.
- If a framework fails to load, every call returns that error once logged, and the sensor records nothing (idle unknown).
- **Needs testing on macOS CI:** the purego signatures, and that a LaunchAgent (Aqua session) gets a non-nil session dictionary.

### Linux

Idle sources are tried in this order. The first that answers is remembered, so steady-state sampling is a single D-Bus call:

1. `org.gnome.Mutter.IdleMonitor.GetIdletime` on the session bus (GNOME on X11 and Wayland).
2. `org.freedesktop.ScreenSaver.GetSessionIdleTime` on the session bus (KDE Plasma, and others that implement it).
3. The X11 `MIT-SCREEN-SAVER` extension (`MsSinceUserInput`) via `github.com/jezek/xgb`. **Only on real X11 sessions**: under Wayland, XWayland only sees input sent to X11 windows.
4. logind `IdleHint` / `IdleSinceHint` for the user's display session (system bus). This is coarse: it only changes when the desktop marks the session idle, often after minutes, and some desktops never set it. That's why it comes last, after X11 (the brief listed it before X11).

If the remembered source fails, the chain falls through to the next one. Every 5 minutes it also re-tries the better sources (for example Mutter starting after the daemon at login). If none works, it logs one warning and re-scans every 5 minutes.

- Session type comes from `XDG_SESSION_TYPE`, `WAYLAND_DISPLAY` and `DISPLAY`. A `systemd --user` unit only sees these when the desktop imports them into the user manager. GNOME and KDE do; the installer's unit may need `PassEnvironment`/`ImportEnvironment` on others.
- D-Bus calls use `NoAutoStart`, so probing never launches a service, and have a 1 s timeout each. Connections are opened lazily, reused and reopened after an error.
- Focus (X11 only): root `_NET_ACTIVE_WINDOW` → that window's `_NET_WM_PID` → `/proc/<pid>/comm` (the kernel process name, at most 15 bytes; never `cmdline`). Window managers without EWMH, or Flatpak apps whose PID is in another namespace, report unknown.
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
| `git config --get user.email` | the repo's effective identity (exit 1 = unset → commits skipped, logged once) |
| `git log HEAD --format=%x1e%H%x1f%at%x1f%ae --shortstat --since=<now-1h> --max-count=200` | your commits: hash, author time, files, insertions, deletions |
| `git diff --shortstat HEAD --` | uncommitted diff size (tracked files; untracked files aren't counted) |

- Only commits whose author email equals `user.email` (case-insensitive) are recorded. Commits by others, such as pulled or merged work, are ignored.
- **Events:** `commit` (Time = author time) for each new hash. `worktree` (Time = now) when the diff size differs from the last snapshot; the first poll of a repo always emits one.
- **Duplicates:** seen hashes live in memory only. A new process, or a repo seen for the first time, re-emits its commits from the last hour, so **the daemon must dedup commit events by (project, hash)**. Hashes are forgotten 2 h after they were first seen, by which time `--since` excludes them.
- State is only updated when every git call for a repo succeeds, so a failed poll is retried in full.
- **Every git process:** `exec.CommandContext` with a 5 s timeout and `WaitDelay` of 1 s; stdout capped at 1 MiB; empty stdin.
  - Environment: `GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`; inherited `GIT_DIR`, `GIT_WORK_TREE`, `GIT_EXTERNAL_DIFF`, `GIT_SSH*`, askpass and `GIT_CONFIG_PARAMETERS` are removed.
  - Flags: `--no-pager -c core.fsmonitor=false -c core.untrackedCache=false -c log.showSignature=false`, plus `--no-ext-diff --no-textconv --no-renames`.
  - Nothing in a repo's config can make the sensor run another program: no fsmonitor hook, external diff, textconv or gpg.
- **Failures** (timeout on a huge repo, "dubious ownership", not a repo) skip that repo for 10 minutes and log once per failure streak.
- **Git missing:** looked up once with `exec.LookPath`. If absent, the sensor logs one warning and idles until shutdown.
- **Bounds:** at most 256 repos tracked; repos inactive for 2 h are forgotten; at most 200 commits per poll.
- Detached HEAD and shallow clones work unchanged (both are tested). Windows paths are passed as `cmd.Dir`, never as arguments.
- **Windows:** git runs with `CREATE_NO_WINDOW`, so no console window flashes every minute.
