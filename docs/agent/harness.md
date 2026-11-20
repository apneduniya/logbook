# Agent: AI harness hooks

This page covers how the agent records AI tool activity: what `logbook install` writes into each AI tool's config, what `logbook hook <harness> <event>` reads, and what it keeps. The design is in [ADR 0009](../adr/0009-local-agent-design.md); the privacy rules are in [ADR 0006](../adr/0006-struggle-engine-on-device.md) and the PRD.

Code: `internal/agent/harness/` (one package per harness, plus shared packages) and `cmd/logbook/cmd_hook.go`.

## How it works

1. `logbook install` calls `harness.All()`. For each harness whose `Detect()` is true, it calls `Install(<absolute path of logbook>)`. Each installer merges Logbook's hook entries into the harness's **user-level** config file. Project files are never touched.
2. When the AI tool fires a hook, it runs `<logbook> hook <harness> <event>` with a JSON payload on stdin.
3. The hook command:
   - reads stdin, keeping at most 1 MiB and discarding the rest (up to 64 MiB, so the tool's write doesn't fail). It waits at most 1 s for stdin to close; on timeout, or when the payload is over 1 MiB, it still uses the top-level scalar fields from the part it read (ids come first in every payload). A prompt cut off by the cap (a giant paste) still reports `PromptChars`: the runes read before the cut, a lower bound of roughly 0.25 to 1 million characters;
   - prints the response the tool's hook protocol requires (see each harness), and nothing else to stdout or stderr;
   - maps the payload to zero, one or two `wire.AIEvent`s (`Time` = now, `SessionID`, `Cwd`, `Project`, `Model`, `Tool` name, `PromptChars`, `Decision`);
   - POSTs them as a JSON array of `wire.LocalEvent` to `http://<local_addr>/local/v1/events` with `X-Logbook-Token`, with a 300 ms timeout and no proxy. `local_addr` must be a loopback IP or `localhost` (sent to 127.0.0.1 directly); any other address is never contacted, so the token and events can't leave the machine in cleartext, and the events are spooled;
   - if that fails for any reason (daemon down or slow, wrong token, no config yet), appends them as JSON lines to `<data dir>/spool/events.jsonl` (dir 0700, file 0600, one `O_APPEND` write per hook so concurrent hooks never interleave lines). The spool is capped at 10 MiB: when full, new events are refused and a warning goes to `agent.log`;
   - always exits 0. A panic is recovered and logged. Problems go to `agent.log` only, because tools read the hook's stdout and stderr (Gemini CLI even parses stderr when stdout is empty).
4. Measured on the dev VPS: ~12 ms per hook with the daemon down (spool path), binary 10 MB.

`Project` is the name of the git repository containing `cwd` (the directory holding `.git`, file or directory), or the base name of `cwd` outside a repository.

### What is never recorded

The parsers decode only the fields they need into typed structs. Prompt text is decoded only as a rune count (`aievent.TextLen`); it is never stored in a Go string field that leaves the parser. These are never read into the event, and tests plant a sentinel in each of them and search every marshalled event and the spool for it:

- prompt text, pasted content, attachments, `initialPrompt`;
- tool input/arguments and tool output/results, including shell commands and file paths in them;
- assistant messages, thinking text, `last_assistant_message`, `prompt_response`, notification messages and details;
- transcript paths, scratchpad paths, session titles, user emails, scheduled-task prompts.

`Cwd` is kept on the device only (the daemon uses it to map events to projects and repos). Text fields are stripped of control characters and cut to the wire limits; a `cwd` longer than 1024 bytes is dropped, not cut.

## Install rules (all harnesses)

- **Merge, don't replace.** Config files are parsed as JSON (JSONC where the tool allows comments). Key order and every value Logbook doesn't own are kept byte for byte (numbers like `2.50` stay as written). Plain JSON files are re-indented with two spaces; JSONC files with comments are edited in place so the comments survive (see Gemini CLI).
- **Logbook's entries are recognised by their command line**: an executable whose base name starts with `logbook`, followed by exactly `hook <harness> <event>`. Exec-form entries (`command` + `args`, or Copilot's `exec` + `args`) and shell strings quoted for sh, cmd.exe or PowerShell are all recognised. Nothing else is ever modified or removed.
- **Idempotent.** Re-installing with the same binary path doesn't rewrite the file. Installing with a new path replaces Logbook's entries one for one. Entries for events Logbook no longer installs are removed.
- **Backups.** Before the first change, the original file is copied to `<data dir>/backups/<harness>.<file name>.orig` (or a `.absent` marker is written if there was no file). Later installs keep that first backup.
- **Uninstall** removes only Logbook's entries. Keys Logbook added (an emptied `"hooks": {}`, Cursor's and Copilot's `"version": 1`) are removed when the backup shows the original didn't have them; a file Logbook created is deleted once nothing else is in it. In a file Logbook created, `"version"` stays as long as the user has added hooks of their own, because Cursor requires it. The backup is not restored automatically, because the user may have changed the file since.
- **Safety.** Writes are atomic (temp file + rename, retried briefly on Windows where the tool may hold the file). Symlinked config files (dotfile managers) are written through, so the link stays. A new file is created 0600; an existing file keeps its mode. Files over 8 MiB, files that aren't valid JSON, and files whose `hooks` isn't an object are left alone with an error.
- The binary path must be absolute, because hooks run in the tool's working directory.

## Claude Code

| | |
|---|---|
| Config | `~/.claude/settings.json`, or `$CLAUDE_CONFIG_DIR/settings.json`. On Windows `~` is `%USERPROFILE%`. |
| Detect | that config dir exists, or `claude` on `PATH` |
| Handler | exec form: `{"type": "command", "command": "<logbook>", "args": ["hook", "claude-code", "<Event>"], "timeout": 10}` in a matcher group without a matcher (all tools). No shell is involved on any OS, so paths with spaces need no quoting, and Windows works with or without Git Bash. Exec form needs Claude Code 2.1.139+; older versions ignore `args` and run `logbook` alone, which prints usage to stderr and exits 0 (harmless). |
| stdout | nothing (exit 0 with no output means "carry on" for every event) |
| Version | hooks reference and changelog read 2026-11-20 (Claude Code 2.1.329) |

| Claude Code event | AIEventKind | Fields used |
|---|---|---|
| `SessionStart` | `session_start` | `session_id`, `cwd`, `model` (only this event carries it, and not always) |
| `UserPromptSubmit` | `prompt_submit` | `session_id`, `cwd`, rune count of `prompt` |
| `PreToolUse` | `tool_start` | `session_id`, `cwd`, `tool_name` |
| `PostToolUse`, `PostToolUseFailure` | `tool_end` | `session_id`, `cwd`, `tool_name` |
| `PermissionRequest` | `permission_ask` | `session_id`, `cwd`, `tool_name` |
| `Stop`, `StopFailure` | `turn_end` | `session_id`, `cwd` |
| `SessionEnd` | `session_end` | `session_id`, `cwd` |

Ignored on purpose: `prompt`, `tool_input`, `tool_response`, `last_assistant_message`, `session_crons[].prompt`, `transcript_path`, `scratchpad_dir`, `session_title`, `message`.

Notes and limitations:
- `PermissionRequest` is used instead of `Notification` (`permission_prompt`), because the notification fires only after ~6 s without typing and so misses quick approvals. Claude Code has no hook for the human's answer, so `Decision` stays empty.
- `SubagentStart`/`SubagentStop` aren't installed: a subagent runs inside the `Agent` tool call, which `PreToolUse`/`PostToolUse` already bracket.
- `UserPromptSubmit` also fires for `/loop` iterations, scheduled tasks, background subagents reporting back and cross-session messages, with no field saying which. A `prompt_submit` is therefore not proof that a human typed; the struggle engine must not treat it as such.
- The model is only known from `SessionStart`; later `/model` switches aren't recorded (`PostModelSwitch` could add this later).
- User hooks wait for the workspace-trust dialog in a new folder, and `--bare`, `disableAllHooks` and managed `allowManagedHooksOnly` turn them off.
- **Cursor also runs `~/.claude/settings.json` hooks.** To avoid double counting, the claude-code parser drops payloads carrying `cursor_version`, and those run with `CURSOR_VERSION` set unless `CLAUDE_CODE_CHILD_SESSION=1` is also set (Claude Code 2.1.172+ sets it on hooks it runs itself, so a real `claude` session started from a shell that inherited Cursor's environment is still recorded). Cursor's docs list `cursor_version` in its own hook payloads but don't say whether its Claude-format payloads keep it, so the env check stays as a fallback. Logbook's own Cursor hooks record those events.
- Future work: Claude Code's OpenTelemetry metrics (`claude_code.active_time.total` with `type="user"` vs `"cli"`, token and cost counters, `prompt.id` correlation) would give a better human/agent split than hooks. That needs a local OTLP receiver in the daemon and setting `CLAUDE_CODE_ENABLE_TELEMETRY` and the exporter env vars, which is out of scope for step 2.

## Codex (CLI, IDE extension, app)

| | |
|---|---|
| Config | `$CODEX_HOME/hooks.json`, default `~/.codex/hooks.json` (`%USERPROFILE%\.codex\hooks.json`). Codex discovers this file by itself, so `config.toml` is never edited and no TOML library is needed. |
| Detect | that dir exists, or `codex` on `PATH` |
| Handler | `{"type": "command", "command": "<quoted logbook> hook codex <Event>", "timeout": 10}` (3 for `SessionEnd` and `Interrupt`, which accept 1 to 3) in a matcher group without a matcher. Codex runs commands with `$SHELL -lc` on macOS/Linux (POSIX quoting) and `%COMSPEC% /C` on Windows (double quotes when the path has spaces or shell characters). |
| stdout | nothing. Codex treats exit 0 with empty stdout as success; `Stop` rejects plain text but skips empty output (checked in `codex-rs/hooks`). |
| Version | `learn.chatgpt.com/docs/hooks` and openai/codex `codex-rs/hooks` on main, read 2026-11-20 |

| Codex event | AIEventKind | Fields used |
|---|---|---|
| `SessionStart` | `session_start` | `session_id`, `cwd`, `model` |
| `UserPromptSubmit` | `prompt_submit` | + rune count of `prompt` |
| `PreToolUse` | `tool_start` | + `tool_name` (`Bash`, `apply_patch`, `mcp__server__tool`, ...) |
| `PostToolUse` | `tool_end` | + `tool_name` |
| `PermissionRequest` | `permission_ask` | + `tool_name` |
| `Stop` | `turn_end` | |
| `Interrupt` | `turn_end` | (the user stopped the turn) |
| `SessionEnd` | `session_end` | |

Every Codex payload carries `model`. Ignored: `prompt`, `tool_input` (including `tool_input.description`), `tool_response`, `last_assistant_message`, `transcript_path`, `turn_id`.

Limitations:
- **Trust step.** Codex skips new or changed non-managed hooks until the user reviews and trusts them once in `/hooks` (CLI) or Settings > Hooks (app). Trust is pinned to the hook's hash, so the definitions stay stable across Logbook updates as long as the binary path doesn't change. `logbook install` should tell the user to do this.
- **Inline hooks in `config.toml`.** Codex docs: "When a layer declares hooks in both `hooks.json` and `config.toml`, Codex loads both and warns at startup." If the user already has `[hooks]`/`[[hooks.<Event>]]` tables in `$CODEX_HOME/config.toml`, Logbook's `hooks.json` makes Codex print that warning on every launch (both sets still run). Logbook never edits `config.toml`; it reads it (a line scan for `[hooks…]` tables or a top-level `hooks` key) and the Codex installer's `Notes()` (the `harness.Noter` interface) returns a message for `logbook install` to print, suggesting the user move their inline hooks into `hooks.json`.
- Subagent hooks report the parent's `session_id`. `PreModelRequest`/`PostModelRequest`, compaction and subagent events aren't installed.
- Hosted tools (web search) don't run tool hooks. Codex Cloud doesn't run local hooks.
- With `fish` as `$SHELL`, a binary path containing a single quote would be mis-quoted (fish escapes differently). Install paths don't contain quotes in practice.

## Gemini CLI

| | |
|---|---|
| Config | `~/.gemini/settings.json`, or `$GEMINI_CLI_HOME/.gemini/settings.json` when `GEMINI_CLI_HOME` is set (Gemini CLI uses it in place of the home directory). User level, so no per-project trust prompt. |
| Detect | that `.gemini` dir exists, or `gemini` on `PATH` |
| Handler | `{"type": "command", "command": "<quoted logbook> hook gemini-cli <Event>", "name": "logbook", "timeout": 5000}` (milliseconds). Gemini CLI runs commands with `bash -c` on macOS/Linux and PowerShell (`-NoProfile -NonInteractive -Command`) on Windows, so a quoted Windows path is written as `& 'C:\...\logbook.exe' hook ...`. |
| stdout | nothing. Gemini parses stdout as JSON and treats empty output as "no decision"; if stdout is empty it parses stderr, which the hook also keeps empty. |
| Version | google-gemini/gemini-cli `docs/hooks/*` and `packages/core/src/hooks/hookRunner.ts` on main, read 2026-11-20 |

| Gemini CLI event | AIEventKind | Fields used |
|---|---|---|
| `SessionStart` | `session_start` | `session_id`, `cwd` |
| `BeforeAgent` | `prompt_submit` | + rune count of `prompt` |
| `BeforeTool` | `tool_start` | + `tool_name` |
| `AfterTool` | `tool_end` | + `tool_name` |
| `Notification` (`notification_type` = `ToolPermission` only) | `permission_ask` | |
| `AfterAgent` | `turn_end` | |
| `SessionEnd` | `session_end` | |

Ignored: `prompt` text, `prompt_response`, `tool_input`, `tool_response`, `llm_request`/`llm_response`, `message`, `details`, `transcript_path`, `timestamp` (the hook stamps its own time, like every harness).

Limitations:
- No hook payload carries the model name. `BeforeModel` has it but also carries the whole request and fires per request, so it isn't installed.
- `settings.json` may contain comments and trailing commas, and it is the user's main Gemini settings file. When it has comments, Logbook edits only the bytes of the top-level `"hooks"` member (replacing its value, adding it before the closing brace, or removing it with its comma), so every comment and the layout outside `"hooks"` are kept; comments *inside* `"hooks"` are lost because that value is re-encoded. The edit is checked to parse back to the intended settings, and Logbook falls back to a full rewrite (comments lost; the backup has them) only if that check fails or the file has `"hooks"` twice.
- `SessionEnd` is best effort: the CLI doesn't wait for it.

## Cursor (editor Agent and Cursor CLI)

| | |
|---|---|
| Config | `~/.cursor/hooks.json`, `{"version": 1, "hooks": {"<event>": [handler, ...]}}` |
| Detect | `~/.cursor` exists, or `cursor`/`cursor-agent` on `PATH` |
| Handler | `{"command": "<quoted logbook> hook cursor <event>", "timeout": 5}`. User hooks run with `~/.cursor` as the working directory, so the path is absolute. On Windows the desktop app runs commands through PowerShell (`& '...'` quoting). |
| stdout | `{"continue":true}` for `beforeSubmitPrompt`, `{}` for the others |
| Version | `cursor.com/docs/hooks` and the CLI changelog (2026.11.17), read 2026-11-20 |

| Cursor event | AIEventKind | Fields used |
|---|---|---|
| `sessionStart` | `session_start` | `conversation_id` (or `session_id`), `model_id` (or `model`), `workspace_roots[0]` as cwd |
| `beforeSubmitPrompt` | `prompt_submit` | + rune count of `prompt` |
| `postToolUse`, `postToolUseFailure` | `tool_start` at now − `duration`, then `tool_end` | + `tool_name`, `cwd` if present, `duration` (ms) |
| `stop` | `turn_end` | |
| `sessionEnd` | `session_end` | |

Ignored: `prompt` text, `attachments`, `tool_input`, `tool_output`, `text` (responses and thoughts), `user_email`, `transcript_path`, `generation_id`, `model_params`.

Why no `preToolUse`: Cursor calls `preToolUse`, `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile`, `beforeTabFileRead` and `subagentStart` "permission hooks". Invalid or schema-mismatched output from them **blocks** the action, and returning `"permission": "allow"` could skip the user's own approval prompts. Logbook installs none of them. Tool start times are instead back-dated from the `duration` that `postToolUse` reports (capped at 6 h), so a tool call yields a `tool_start`/`tool_end` pair. The only events whose `Time` isn't "now" are these back-dated starts.

Limitations:
- Cursor has no hook for "waiting for the human to approve", so it never produces `permission_ask`.
- Tab completions (`afterTabFileEdit`) and agent file edits (`afterFileEdit`) aren't recorded in step 2; they're useful AI-vs-typed signals for later.
- Cloud agents never run user hooks. Cursor also runs `~/.claude/settings.json` hooks; see the Claude Code section for how double counting is avoided.

## GitHub Copilot (Copilot CLI, VS Code agent, Copilot app)

Copilot CLI, VS Code's Copilot agent harness and the Copilot app share one hook system that reads user hook files from `~/.copilot/hooks/*.json`.

| | |
|---|---|
| Config | Logbook owns `~/.copilot/hooks/logbook.json` (`$COPILOT_HOME/hooks/logbook.json`; `%USERPROFILE%\.copilot\hooks\` on Windows). No user file is edited. |
| Detect | that dir exists, or `copilot` on `PATH` |
| Handler | exec form: `{"type": "command", "exec": "<logbook>", "args": ["hook", "copilot", "<event>"], "timeoutSec": 5}` (no shell) |
| stdout | nothing ("return `{}` or empty for no action") |
| Version | `docs.github.com/en/copilot/reference/hooks-reference` and microsoft/vscode-docs `agent-customization/hooks.md`, read 2026-11-20 |

| Copilot event (camelCase payloads) | AIEventKind | Fields used |
|---|---|---|
| `sessionStart` | `session_start` | `sessionId`, `cwd` |
| `userPromptSubmitted` | `prompt_submit` | + rune count of `prompt` |
| `postToolUse`, `postToolUseFailure` | `tool_end` | + `toolName` |
| `notification` (`notification_type` = `permission_prompt` only) | `permission_ask` | |
| `agentStop` | `turn_end` | |
| `sessionEnd` | `session_end` | |

Ignored: `prompt` text, `initialPrompt`, `toolArgs`, `toolResult`, `message`, `title`, `transcriptPath`, `timestamp`.

Limitations, stated honestly:
- **No `tool_start`.** Copilot's `preToolUse` command hooks fail *closed*: a crash or non-zero exit denies the tool call. If Logbook's binary were deleted without `logbook uninstall`, every Copilot tool call would be blocked, so Logbook doesn't install `preToolUse`. Tool calls appear only as `tool_end`.
- Payloads carry no model name.
- **What isn't covered**: Copilot inline completions (ghost text) and Next Edit Suggestions don't run hooks; VS Code's deprecated "Local" chat harness uses `chat.hookFilesLocations` instead; JetBrains, Visual Studio and Xcode Copilot have no documented hooks. Editor heartbeats still record that time. A VS Code extension that labels these as AI time is the planned route for those gaps.
- Copilot in the cloud runs only repository hooks, never user hooks.

## OpenCode

OpenCode has no command-hook config, only JavaScript/TypeScript plugins, so Logbook installs a small plugin.

| | |
|---|---|
| Config | `$XDG_CONFIG_HOME/opencode/plugins/logbook.js`, default `~/.config/opencode/plugins/logbook.js` (OpenCode uses XDG paths on every OS, so `%USERPROFILE%\.config\opencode` on Windows). OpenCode loads `.js` files from its global plugin directory automatically; `opencode.json` is not edited. |
| Detect | that config dir exists, or `opencode` on `PATH` |
| File | written whole by Logbook and marked `logbook-managed-plugin` on its first line; mode 0644. Install refuses to replace a file without the marker; uninstall removes it only if the marker is there. |
| stdout | nothing (the plugin ignores it) |
| Version | OpenCode V2 docs (`/docs/build/plugins/server`, `/docs/build/plugins/migrate-v1`) and anomalyco/opencode `packages/schema` on `dev`, read 2026-11-20. V1 via the object form's `server()`, supported from 1.18.29. |

The plugin (`internal/agent/harness/opencode/plugin.go`) default-exports `{id: "logbook", setup, server}`: V2 calls `setup(ctx)`, V1 calls `server()`. For each event it spawns `<logbook> hook opencode <event>` in the background (`stdio` ignored, killed after 5 s, never awaited) and writes a metadata-only JSON object to its stdin: `session_id`, `cwd` (V2: the session's `location.directory` from `session.created`; V1: the plugin instance's directory), `model` (tracked per session from `session.created`/`session.model.selected` or V1 `chat.message`), `tool`, `prompt_chars` (counted in the plugin as Unicode code points) and `decision`. Prompt text, tool arguments and output never leave the plugin.

| OpenCode source (V2 / V1) | Plugin event | AIEventKind |
|---|---|---|
| event `session.created` / event `session.created` | `session-start` | `session_start` |
| `session.hook("prompt")` / `chat.message` | `prompt` | `prompt_submit` (with `prompt_chars`) |
| `tool.hook("execute.before")` / `tool.execute.before` | `tool-start` | `tool_start` |
| `tool.hook("execute.after")` / `tool.execute.after` | `tool-end` | `tool_end` |
| event `permission.asked` / `permission.asked`, `permission.updated` | `permission-ask` | `permission_ask` |
| event `permission.replied` (`reply`) / `permission.replied` (`response`) | `permission-reply` | `permission_ask` with `Decision`: `reject` → `deny`, `once`/`always` → `allow` |
| events `session.execution.succeeded/failed/interrupted` / `session.idle` | `turn-end` | `turn_end` |
| event `session.deleted` / `session.deleted` | `session-end` | `session_end` |

A `permission_ask` with a non-empty `Decision` is the answer to the previous ask in that session, not a new ask.

One OpenCode server can host sessions in several directories (the desktop app does), and in V2 `ctx.event.subscribe` streams the whole server's events while `ctx.location` is only the plugin instance's location. So each V2 plugin instance reports only the sessions it owns: a `session.created` whose `location.directory` equals `ctx.location.directory`, or a session its own location-scoped `prompt`/tool hooks have fired for (sessions created before the plugin loaded). Events for any other session are ignored, so each event is reported once, with the right `cwd`, even when the global plugin is loaded once per location.

Limitations:
- OpenCode sessions don't really end; `session_end` is only sent when a session is deleted.
- V1 releases before 1.18.29 expect function exports and won't load this plugin.
- The V1 event field names come from the V1 docs and are handled defensively; the Node test drives both paths with fake contexts, but no live OpenCode was run.

## Testing

- Golden install tests per harness (`testdata/install_*.golden`): missing config, existing config with other hooks and unknown keys, re-install (no change, no new backup), new binary path, uninstall (semantically equal to the original, or file removed if Logbook created it). Regenerate with `LOGBOOK_UPDATE_GOLDEN=1 go test ./internal/agent/harness/...`.
- Parser tests use payload fixtures built from each tool's documented examples, with `SENTINEL-PRIVATE` planted in every private field; the marshalled events must not contain it.
- `hookrun` tests run every harness's hook with the daemon down (spool written, exact stdout, < 1 s) and with stdin over 1 MiB. `cmd/logbook` tests run the real command with `LOGBOOK_HOME` set.
- `TestPluginInNode` runs the OpenCode plugin under Node (skipped when `node` is missing) and checks the calls a fake `logbook` receives.
- No AI tool was run live on the dev VPS; payload shapes come from the vendors' docs and, for Codex and Gemini CLI, their source. Windows and macOS behaviour (PowerShell/cmd quoting, exec form) is covered by unit tests on Linux and still needs a pass on real machines in CI.
