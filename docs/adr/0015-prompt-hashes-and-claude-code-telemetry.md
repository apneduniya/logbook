# 0015: Keyed prompt hashes and a local receiver for Claude Code's active time

- Status: accepted
- Date: 2026-11-20

## Context

Two crediting rules need signals the hooks didn't record (docs/STRUGGLE-RULES.md):

- `loop` cuts the 3rd and later sends of the same prompt in a session. The engine has to know two prompts are the same without Logbook ever keeping prompt text.
- `otel_mismatch` compares credited minutes with Claude Code's own measure of human time, `claude_code.active_time.total` (`type` = `user` or `cli`). Claude Code only exports it over OpenTelemetry (OTLP). Every datapoint also carries the user's email, account and organization ids by default.

## Decision

**Prompt hashes.**
- A hook that sees prompt text records `PromptHash = hex(HMAC-SHA256(prompt_key, SHA-256(normalise(prompt))))` on `prompt_submit` events. The text is dropped inside the JSON decoder (`aievent.Prompt`); only its length and digest leave it.
- `normalise`: drop invalid UTF-8, trim, collapse every run of Unicode white space to one space, lowercase rune by rune (Go's `strings.ToLower`). No Unicode NFC: it would put `golang.org/x/text` tables into every hook process, and the rule compares prompts within one session of one tool, where the same request arrives in the same encoding.
- `prompt_key = HKDF-SHA256(local_token, no salt, info "logbook prompt hash v1")`, derived in the hook process from `config.json`. A keyed hash can't be checked against guessed prompts without the key, and hashes from two devices can't be compared.
- OpenCode's plugin computes the SHA-256 of the normalised prompt in JavaScript and sends only that digest; the hook keys it. A test runs the plugin under Node and checks its digests equal Go's for tricky input (NBSP, ideographic space, dotted capital I, final sigma).
- A prompt cut off by the 1 MiB payload cap keeps its length but gets no hash.

**OTLP receiver.**
- The daemon's loopback server accepts OTLP/HTTP **JSON** at `POST /v1/metrics` (package `otlp`). Protobuf gets 415 and one log line: a pure-Go protobuf decoder would mean a new dependency for one metric, and the installer controls the protocol.
- `POST /v1/logs` is accepted and discarded. The installer never points logs at the agent; this only stops retries if a user points a generic endpoint here. Prompts are already counted by hooks.
- Requests need `X-Logbook-Token: <otlp token>`, set through `OTEL_EXPORTER_OTLP_METRICS_HEADERS` (Claude Code merges per-signal headers into the request; checked in Claude Code's monitoring docs, 2026-11-20). The **OTLP token** is `hex(HKDF-SHA256(local_token, info "logbook otlp token v1"))`, not the local token: `settings.json` is often published in dotfiles repos, and a leaked OTLP token reveals neither the local token (which accepts hook events) nor the prompt key.
- The decoder reads only three attribute keys, `session.id`, `type` and `vcs.repository.name`, from datapoints (falling back to the resource). Every other attribute, including `user.email`, `user.account_uuid`, `user.account_id`, `user.id`, `organization.id`, `terminal.type` and host details, is skipped without being decoded.
- Claude Code's default temporality is delta. Cumulative series are converted to deltas per (`session.id`, `type`, start time) series using the last stored point, so an agent restart doesn't double count; a lower total with the same start is a counter reset; a resent point is skipped. Points ending in the future, starting more than 90 days ago, or with more seconds than their interval allows (plus 1 minute) are rejected.
- Points live in a local `active_time` table (90 days, same row cap as raw events), never uploaded. `localdb.ActiveTimes` feeds them to the engine; a session's project directory comes from its hook events (same `session.id`).
- Claude Code sends nothing for idle intervals, so lost exports would look like zero time. The daemon records when its receiver runs (`otlp_uptime`, a mark every 30 s), and `ActiveTimes` leaves out any session whose span in the window (hook events and points) overlaps a time the receiver wasn't running, with margins of 2 minutes before a run's start and 1 minute after its last mark. The engine's input type is unchanged; incomplete sessions simply aren't offered to `otel_mismatch`.

**Installer.** `logbook install` adds to the `env` of `~/.claude/settings.json`: `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_METRICS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/json`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:<port>/v1/metrics`, `OTEL_EXPORTER_OTLP_METRICS_HEADERS=X-Logbook-Token=<otlp token>` and `OTEL_METRIC_EXPORT_INTERVAL=60000`.
- Metrics-only variables, so logs and traces stay off and a user's generic exporter settings are never touched.
- `OTEL_METRICS_INCLUDE_REPOSITORY` is **not** set. It would widen what Claude Code collects (repository URL and owner) for every exporter, and the session id already joins OTel points to hook events, which know the directory. The receiver still keeps `vcs.repository.name` if the user turned it on.
- Anyone else's telemetry wins. If any managed source (system `managed-settings.json` and `managed-settings.d/`, the server-managed cache `remote-settings.json`, macOS managed preferences, the Windows `HKLM`/`HKCU` policy value) has a telemetry entry or `otelHeadersHelper`, or can't be read, Logbook adds nothing and removes every entry it wrote. Claude Code strips developer-set per-signal endpoints and headers under managed OTLP endpoints or credentials but keeps exporter selectors, so Logbook's selectors alone would send the user's metrics, with their email and account ids, to the administrator's collector.
- If `settings.json` has any `OTEL_*` entry or `otelHeadersHelper`, or one of Logbook's keys with another value, or the install environment sets an OTLP variable, Logbook adds nothing and prints a note. When that is the user's own exporter added after install, the enable flag and metrics exporter Logbook wrote are handed over (left in place and recorded as such) instead of removed, because the user's setup may rely on them; only the entries pointing at the agent go.
- The daemon re-runs these checks against `settings.json` and the managed sources every 10 minutes, so a later setup isn't overridden until the next install.
- A sidecar (`<data dir>/backups/claude-code.telemetry.json`) records which entries Logbook added; uninstall removes exactly those, only while they still hold Logbook's value.

## Consequences

- The `loop` rule works across every harness that sees prompt text, with no prompt text stored anywhere.
- Changing the normalisation or key derivation changes every hash: it needs a new info string (`v2`) and a rules version bump.
- Claude Code's active time is available on users' machines when they have no telemetry of their own; users with their own OTel setup don't get the `otel_mismatch` cross-check.
- Claude Code reads `env` from `settings.json` at start, so sessions started before install export nothing until restarted.
- On machines whose managed settings configure telemetry, Logbook stays out and doesn't get Claude Code's active time; hooks still work. Managed settings added after install are honoured within 10 minutes, but Claude Code sessions already running keep their exporter until restart.
- A session that overlapped an agent stop gets no `otel_mismatch` check for windows covering the stop.
