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

## Implementation notes (rules v1)

`pkg/struggle` implements the rules above. Where the text above leaves room, the code decides as follows; ADR 0012 records the output contract. Golden scenarios in `pkg/struggle/testdata` pin every rule.

**Window and context**
- `Window.From` and `Window.To` are truncated down to whole UTC minutes; a window longer than 7 days is cut to its first 7 days.
- Events outside the window are context only. No rule looks back more than 30 minutes, and none looks more than 30 minutes ahead (the age at which a minute is final). Callers include at least 30 minutes of events before `From`; the server, when it re-runs closed history, also includes 30 minutes after `To`.
- Output rows are sorted by (minute, project). Every `you` and `agent` minute is emitted, and every `idle` minute that has a project. An idle minute with no project (nothing happened in the last 5 minutes) is not emitted, so a missing minute means idle.

**Present**
- Each activity sample gives the instant of the last OS input: sample time minus idle seconds. A minute is present when such an instant falls inside it. This is the precise form of "idle < 60 s at a sample in the minute", which would credit up to a minute after the last input.
- NaN, negative or infinite idle readings are ignored. Readings are capped at 10⁷ s.

**Engaged**
- Present in the minute or in one of the 3 minutes before it, and the focus is allowed.
- The focus is allowed if any sample in the minute shows an editor, terminal, AI app, `unknown`, or a browser during an active session. `other` isn't allowed.
- With no sample in the minute, the latest sample of the 2 minutes before decides. With none at all, the focus is allowed: a missing focus signal is never held against the person.
- Active session: an editor heartbeat or AI event, for any project, in the minute or the 15 minutes before it.

**Low-signal fallback**
- "No working idle source" means no activity sample within 2 minutes either side of the minute. The fallback is decided per minute, so a sensor that stalls for a few minutes is covered too.
- Presence then comes from human heartbeats in the minute. A human heartbeat is one not attributed to AI. It counts if it is a write, if its line or cursor differs from the previous human heartbeat's (when both are known), or if it is for a different file. The 3-minute grace applies as usual, and `you` rows are flagged `low_signal`.
- `background_editor`: a low-signal minute that has heartbeats, but no human write or movement in it or in the 4 minutes before it, and is neither engaged nor agent-busy. It is labelled `idle` with cut `background_editor` instead of `idle`.

**Turns and agent busy**
- A turn starts at `prompt_submit`, or at a `tool_start` outside any turn. Sessions are keyed by harness and session id.
- A turn ends at the first of these:
  - `turn_end`;
  - `session_end`;
  - the session's next `prompt_submit`, an interrupt (Claude Code sends no `Stop` on interrupt);
  - its last event, when the session then has no events for more than 10 minutes.
- At the end of the data, a turn whose last event is less than 10 minutes old is still running. "The end of the data" is `To` or the latest AI event, whichever is later.
- From a `permission_ask` until the session's next other event, the agent is waiting, not busy.
- Events with the same timestamp are ordered along the lifecycle: `session_start`, `prompt_submit`, `tool_start`, `permission_ask`, `tool_end`, `turn_end`, `session_end`.
- A turn touches every minute its span overlaps, including the minute it ends in, unless it ends exactly on that minute's first second.

**Projects**
- AI events take the project of the longest repo path containing their cwd. Matching is on path boundaries, with `\` read as `/`, and Windows drive paths compared case-insensitively. Without a match they keep `AIEvent.Project`.
- Heartbeats are matched on their file path first, and keep `Heartbeat.Project` otherwise. Events without a project don't count for attribution.
- `you` minute with project events in it: 60 s are split by event count, by largest remainder, with ties going to the smaller project name. A project left with 0 s is dropped. Every heartbeat and every AI event counts as one event.
- `you` minute with no event in it: it goes to the latest project of the last 5 minutes. If there is none, it goes to the latest project of the active session (15 minutes), for example while reading docs in the browser. Otherwise the project is `""`.
- `agent` minute: it goes to the project of the latest-starting busy turn (the project of that turn's first event that has one), or to the last 5 minutes' project.

**Cuts**
- A cut sets every `you` row of each minute the cut turn touches to 0 credit, whatever its project, and adds the cut code. The label stays `you`.
- `rubber_stamp`: a stamp is a prompt that meets all of these:
  - it is sent 0 to 15 s after the previous turn of its session ended with `turn_end`;
  - it is 1 to 25 characters long (an unknown length of 0 is never a stamp);
  - no human heartbeat came in between.

  Three or more stamps in a row make a streak. The turns of every stamp after the first are cut. The streak is confirmed when its third stamp arrives, and the second stamp's turn is then cut back to at most 30 minutes before the third, so the rule never changes a final minute.
- `loop`: a prompt is cut when its session had at least 2 earlier prompts with the same keyed hash in the 30 minutes before it, inclusive. This catches the 3rd and later repeats. An empty hash never matches.

**Flags**
- `automated_session`: a `prompt_submit` with no OS input instant in the 2 minutes before it (up to 2 s after it, for sampler delay). Prompts with no activity sample within 2 minutes are not judged, because missing data isn't evidence. The flag goes on all rows of the turn's minutes.
- `scripted_input`: 30 consecutive intervals with a population standard deviation under 0.2 s. It is checked on two streams:
  - heartbeat intervals between 1 s and 115 s. Faster than the WakaTime 2-minute throttle, so steady typing, which arrives at ~120 s intervals, can't trigger it, and bulk duplicates (0 s) don't either.
  - OS input instants that a later sample saw at least 2 s of quiet after, with intervals between 2 s and 10 min. Observations of the same instant within 0.5 s are merged. Continuous typing is seen at the sampler's own 5 s rhythm and never has 2 s of quiet, so it is skipped; a jiggler's fixed period is caught.

  The flag goes on all rows of the minutes from the first to the last event of the run, reaching back at most 30 minutes from the run's last event so it never lands on a final minute.
- `otel_mismatch`: computed for each session that has active-time data. The harness defaults to `claude-code`.
  - The session's minutes are the window's minutes in which the session had an AI event or a turn, and in which the dominant focus was a terminal, AI app or unknown. Time spent in the editor while the agent works isn't compared, because Claude Code can't see it.
  - Credited = the credited seconds of those minutes.
  - User = the session's `UserSecs`, prorated by each datapoint's overlap with the window.
  - The flag goes on the `you` rows of those minutes when user < 50 % of credited and credited − user > 10 min.
- `long_day`: a running credited total per local day (`Input.TZ`, UTC if nil), counted within the window. Credited rows are flagged from the minute the total passes 12 h. The server re-checks over whole days.
- `otel_mismatch` and `long_day` depend on the window. The other flags, and every label, credit and cut, don't, once a minute is 30 minutes older than the end of the data. A property test replays every scenario as the agent sees it, minute by minute, and checks this.

**Determinism and cost**
- The engine sorts copies of every input with a total order and drops exact duplicates. Maps are used only for lookups, never to order output. `time.Now` is never read.
- A 2-hour window with 30 minutes of context takes about 5.5 ms on a 4-core 2020s server vCPU (`BenchmarkCompute2h`). The input has activity every 5 s, heartbeats every ~30 s, an agent turn every 5 minutes with a tool call every ~10 s, and OTel points. Memory is linear in events plus minutes.
