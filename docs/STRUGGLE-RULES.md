# Crediting rules (struggle engine), rules v1

Draft, 20 Nov 2026. Implemented by `pkg/struggle`; the decision record is ADR 0010.

Logbook doesn't try to measure "struggle" directly; nobody can. It starts from the time a person is actually present and working, labels every minute, and **removes only what is clearly not productive struggle**. Every cut gives its reason. These rules are public and versioned, and the same version is re-run on the server for every submission, so anyone can check them.

## Principles

1. **The same rules for AI and hand-coding.** You can struggle productively with Claude, and you can coast while typing code yourself.
2. **Never penalise beginners.** No rule looks at skill, project difficulty, code quality, token counts or which model was used.
3. **Hardware input is the main signal that a person is there.** Editor heartbeats and AI hook events say what is happening; OS input timing says that a human is at the keyboard.
4. **Cut what is clearly not struggle; flag what only looks odd.** Cuts change credited time. Flags change nothing on their own; they point reviewers at minutes worth checking.
5. **Conservative, but explainable.** If a minute is cut, the user can see which rule cut it and why.

## Signals (all recorded on the device)

| Signal | Source | What it tells us |
|---|---|---|
| Activity samples | OS idle time and focused app, every 5 s | Whether a human is touching the machine, and in which kind of app (editor, terminal, AI app, browser) |
| Editor heartbeats | WakaTime plugins through the agent | Which file and project, writes, cursor movement, AI-attributed line changes |
| AI events | Harness hooks | Prompt sent, agent working (tool start/end), waiting for permission, turn finished; prompt **length** and a **keyed hash** only |
| Claude Code active time | Claude Code OpenTelemetry (`claude_code.active_time.total`, `type` = user/cli), optional | Claude Code's own measure of human vs agent time, used as a cross-check |
| Git events | System `git` | Commits and diff sizes. Shown to reviewers only; **never used to credit or cut minutes**, so nobody is pushed to commit for the sake of it |

## Labelling each minute

For every minute, per device:

1. **Present:** some human input happened within the minute (OS idle time < 60 s at any sample in the minute).
   - If the device has no working idle source (some Wayland desktops, headless machines), fall back to editor heartbeats with a write or cursor movement in the minute. Mark the minute `low_signal`.
2. **Engaged:** present now, or present at some point in the last **3 minutes** (thinking and reading time) *and* the focused app is an editor, terminal, AI app, or a browser during an active session.
   - An active session means an editor heartbeat or AI event for some project within the last 15 minutes.
   - A browser outside an active session doesn't count.
3. **Agent busy:** an AI session has an open turn, between `prompt_submit` or `tool_start` and `turn_end`. A turn with no events for 10 minutes is treated as ended.
4. **Label:**
   - `you` if engaged;
   - otherwise `agent` if an agent is busy;
   - otherwise `idle`.
5. **Project:** a `you` minute goes to the project of the most recent heartbeat or AI event in the last 5 minutes. AI events are matched by repository path, not folder name. If two projects had events in the same minute, the seconds are split by event count.

Only `you` minutes earn credit, up to 60 seconds each. A person watching the agent work (input within 3 minutes, focus on the terminal or AI app) is `you`. A person who walked away while the agent works is `agent`.

## Cuts (rules v1)

| Code | Rule | Effect |
|---|---|---|
| `idle` | Not engaged and no agent busy | 0 credit (label `idle`) |
| `agent_alone` | Agent busy, person not engaged | 0 credit (label `agent`) |
| `rubber_stamp` | ≥ 3 turns in a row where the next prompt follows `turn_end` within 15 s, the prompt is ≤ 25 characters ("yes", "continue"), and there's no editor activity in between | Minutes in that streak after the first are credited 0 |
| `loop` | The same prompt (same keyed hash) sent ≥ 3 times within 30 min in one session | Minutes of the 3rd and later repeats' turns are credited 0 |
| `background_editor` | (Low-signal devices only) heartbeats with no write and no cursor movement for > 5 min | Those minutes credited 0 |

## Flags (no change to credit; shown to reviewers)

| Code | Rule |
|---|---|
| `scripted_input` | Heartbeat or input timing too regular for a human (e.g. interval standard deviation < 0.2 s over 30+ events) |
| `automated_session` | An AI session with turns starting with no human presence before them (`prompt_submit` with no input in the previous 2 min), e.g. `/loop`, scheduled tasks, SDK runs |
| `otel_mismatch` | Claude Code's own `active_time` (user) is far below the `you` minutes credited during its sessions |
| `long_day` | More than 12 credited hours in one local day |
| `low_signal` | Minutes credited without an OS input signal |
| `multi_device` | Credited minutes overlap on two devices (checked on the server) |

## What gets uploaded

- **Continuously:** labelled minutes per project, with credited seconds, cut and flag codes, and the rules version. Minutes are final once they're 30 minutes old.
- **Daily:** one signed evidence hash per device per day (ADR 0010). It covers the canonical encoding of that day's raw events, chained to the previous day.
- **Never, unless the user submits:** the raw events themselves.

## Tuning

The thresholds (3 min, 15 s, 25 characters, and so on) are first guesses. They'll be tuned on real sessions from the team and the pilot program, and every change bumps the rules version. Submitted hours are always re-checked with the version that produced them.
