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
| Claude Code active time | Claude Code OpenTelemetry (`claude_code.active_time.total`, `type` = user/cli), optional | Claude Code's own measure of active time (user: keyboard interactions; cli: tool execution and AI responses), used as a cross-check |
| Git events | System `git` | Commits and diff sizes. Shown to reviewers only; **never used to credit or cut minutes**, so nobody is pushed to commit for the sake of it |

## Labelling each minute

For every minute, per device:

1. **Present:** the person touched the machine in or just before the minute: OS idle time < 60 s at some sample in the minute.
   - If the device has no working idle source (some Wayland desktops, headless machines), fall back to editor heartbeats with a write or cursor movement in the minute. Mark the minute `low_signal`.
2. **Engaged:** present now, or present at some point in the last **3 minutes** (thinking and reading time) *and* the focused app is an editor, terminal, AI app, or a browser or unrecognised app during an active session.
   - An active session means one of the person's own signals within the last 15 minutes: an editor heartbeat not attributed to AI, a prompt the person sent, or an answer to a permission ask. An agent's events alone don't keep a session active, so a running agent doesn't make browsing count.
   - A browser outside an active session doesn't count.
   - Input with no project and no active session (nothing to say any project is being worked on) isn't engaged.
3. **Agent busy:** an AI session has an open turn, between `prompt_submit` (or the agent's first tool call or permission ask, when no prompt was seen) and `turn_end`. A turn with no events for 10 minutes is treated as ended.
4. **Label:**
   - `you` if engaged;
   - otherwise `agent` if an agent is busy;
   - otherwise `idle`.
5. **Project:** a `you` minute goes to the project of the most recent heartbeat or AI event in the last 5 minutes. AI events are matched by repository path, not folder name. If two projects had events in the same minute, the seconds are split by the person's own signals (human heartbeats, prompts, permission answers), and only when there are none by all events, so an agent's tool calls never outweigh the person's own editing.

Only `you` minutes earn credit, up to 60 seconds each. A person watching the agent work (input within 3 minutes, focus on the terminal or AI app) is `you`. A person who walked away while the agent works is `agent`.

## Cuts (rules v1)

| Code | Rule | Effect |
|---|---|---|
| `idle` | Not engaged and no agent busy | 0 credit (label `idle`) |
| `agent_alone` | Agent busy, person not engaged | 0 credit (label `agent`) |
| `rubber_stamp` | ≥ 3 turns in a row, within 30 min, where the next prompt follows `turn_end` within 15 s, the prompt is ≤ 25 characters ("yes", "continue"), and there's no editor activity in between | Minutes in that streak after the first are credited 0 |
| `loop` | The same prompt (same keyed hash, longer than 25 characters) sent ≥ 3 times within 30 min in one session, with no editor activity by the person since the earlier sends | Minutes of the 3rd and later repeats' turns are credited 0 |
| `background_editor` | (Low-signal devices only) heartbeats with no write and no cursor movement for > 5 min | Those minutes credited 0 |

`rubber_stamp` and `loop` cut a turn's minutes for at most 25 minutes after its prompt, and never a minute in which the person edited code themselves.

## Flags (no change to credit; shown to reviewers)

| Code | Rule |
|---|---|
| `scripted_input` | Heartbeat or input timing too regular for a human (e.g. interval standard deviation < 0.2 s over 30+ events) |
| `automated_session` | An AI session with turns starting with no human presence before them (`prompt_submit` with no input in the previous 2 min), e.g. `/loop`, scheduled tasks, SDK runs |
| `otel_mismatch` | Claude Code's own `active_time` (user + cli) is far below the `you` minutes credited during its sessions |
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

**Window, context and finality**
- `Window.From` and `Window.To` are truncated down to whole UTC minutes; a window longer than 7 days is cut to its first 7 days.
- Events outside the window are context only. A minute's label, credit, cuts and window-independent flags depend only on events from **60 minutes before** the minute's start (`struggle.ContextBefore`) to **25 minutes after** it (`struggle.Lookahead`). So:
  - the agent computes the last 60 minutes from the last 90 minutes of events; whenever it rewrites a minute that is 25 to 30 minutes old, the value is final;
  - the server, re-running closed history, passes events from 60 minutes before `From` to 30 minutes after `To` and sets `Input.DataEnd` to that end.
- "The end of the data" (for turns still running, below) is the latest of `To`, `Input.DataEnd` and every event.
- Output rows are sorted by (minute, project). Every `you` and `agent` minute is emitted, and every `idle` minute that has a project. An idle minute with no project (nothing happened in the last 5 minutes) is not emitted, so a missing minute means idle.

**Present**
- A minute is present when an activity sample in it reports OS idle time under 60 s, as the rule says. A single input therefore makes its own minute present, and the next one too when a sample there still sees it within 60 s.
- NaN, negative or infinite idle readings are ignored. Readings are capped at 10⁷ s.

**Engaged**
- Present in the minute or in one of the 3 minutes before it, and the focus is allowed.
- The focus is allowed if any sample in the minute shows an editor, terminal or AI app, or a browser or `unknown` focus (the sensor reports `unknown` when it can't read the focused app; an unrecognised `FocusKind` is read the same way) during an active session. `other` isn't allowed.
- With no sample in the minute, the latest sample of the 2 minutes before decides. With none at all, the focus is allowed: a missing focus signal is never held against the person.
- Active session: one of the person's own signals in the minute or the 15 minutes before it. These are: a heartbeat not attributed to AI; a `prompt_submit` that isn't flagged `automated_session`; an answer to a permission ask (the session's next event after a `permission_ask`, or a `permission_ask` carrying a `Decision`, which is how OpenCode reports the reply). A `permission_ask` with a `Decision` always counts, even when the ask it answers is before the caller's data or its turn has timed out; a reply that also ends a wait counts once.
- An engaged minute with no project (below) and no active session is not engaged: keyboard use with no editor or AI activity at all isn't evidence of work.

**Low-signal fallback**
- "No working idle source" means no activity sample within 2 minutes either side of the minute. The fallback is decided per minute, so a sensor that stalls for a few minutes is covered too.
- Presence then comes from human heartbeats in the minute. A human heartbeat is one not attributed to AI. It counts if it is a write, or if its line or cursor differs from the person's last heartbeat **on the same file** within 30 minutes (both known). Switching to another file is not movement: editors also send heartbeats when files change on disk. The 3-minute grace applies as usual, and `you` rows are flagged `low_signal`.
- `background_editor`: a low-signal minute that has heartbeats, but no human write or movement in it or in the 4 minutes before it, and is neither engaged nor agent-busy. It is labelled `idle` with cut `background_editor` instead of `idle`.

**Turns and agent busy**
- A turn starts at `prompt_submit`. Sessions are keyed by harness and session id.
- A mid-turn event outside any turn (`tool_start`, `tool_end` or `permission_ask`, answered or not) starts an *inferred* turn at that event: the agent is working, and its prompt is before the caller's data, or the harness never sent one. After an unanswered ask the inferred turn is waiting. So whether a session is busy at a moment depends only on its last event before it, at most 10 minutes back, never on a prompt further back. GitHub Copilot needs this: its hooks send `tool_end` but no `tool_start`, so a long Copilot turn is otherwise known only to callers holding its prompt.
- An inferred turn is never judged by a rule decided at a prompt: `loop`, `rubber_stamp` and `automated_session` read only turns started by a `prompt_submit`. The turn of a prompt still covers at most 25 minutes after it (below), so a caller that sees an inferred turn where the full history has a prompt turn agrees with it on every minute it computes. An inferred turn's agent minutes take their project from its recent events, like any turn.
- `session_start` is not a turn event: it neither starts, extends, answers nor ends a turn (Claude Code also sends it on resume and on compaction mid-turn).
- A turn ends at the first of these:
  - `turn_end`;
  - `session_end`;
  - the session's next `prompt_submit`, an interrupt (Claude Code sends no `Stop` on interrupt);
  - its last event, when the session then has no events for more than 10 minutes.
- At the end of the data, a turn whose last event is less than 10 minutes old is still running.
- Every AI event is stamped when its hook runs, so it reaches the agent before any minute it can change is final. Cursor reports a tool call only when it ends, so a call is one `tool_end` at that moment, with no `tool_start`. A call that runs for more than 10 minutes without other events is therefore a gap: the turn times out at its last event before the call, and the call's `tool_end` starts an inferred turn. The session doesn't make the minutes of the gap agent-busy, and no `loop`, `rubber_stamp` or `automated_session` cut reaches into them, because the turn whose prompt decided the cut ended before the call. A `tool_start` back-dated by the call's duration would arrive up to 6 hours late and change minutes already final.
- From a `permission_ask` until the person answers (the session's next event other than a new ask), the agent is waiting, not busy.
- Events with the same timestamp are ordered along the lifecycle: `session_start`, `prompt_submit`, `tool_start`, `permission_ask`, `tool_end`, `turn_end`, `session_end`.
- A turn touches every minute its span overlaps, including the minute it ends in, unless it ends exactly on that minute's first second.

**Projects**
- AI events take the project of the longest repo path containing their cwd. Matching is on path boundaries, with `\` read as `/`, and Windows drive paths compared case-insensitively. Without a match they keep `AIEvent.Project`.
- Heartbeats are matched on their file path first, and keep `Heartbeat.Project` otherwise. Events without a project don't count for attribution.
- Every project name (repo projects, `Heartbeat.Project`, `AIEvent.Project`) is normalised the way the server stores it (`pkg/projectname`): control characters removed, surrounding white space trimmed, and WakaTime's `<<LAST_PROJECT>>` read as no project. This happens before anything is counted, so `app` and ` app ` are one project and the engine's rows are already in the server's form.
- `you` minute with project events in it: 60 s are split by the count of the person's own signals per project (human heartbeats, prompts, permission answers); only when the minute has none of those, by the count of all heartbeats and AI events. The split is by largest remainder, with ties going to the smaller project name. A project left with 0 s is dropped. A minute has at most **8 projects** (`struggle.MaxProjectsPerMinute`, the server's limit per device-minute): with more, only the 8 with the most signals are kept (ties: the smaller name) and the 60 s are split among them by the same counts, so the dropped projects' share goes to the kept ones in proportion and the minute keeps its full credit.
- `you` minute with no event in it: it goes to the latest project of the last 5 minutes, else of the last 15 minutes (for example while reading docs in the browser), else of the person's first signal in the next 5 minutes (for example while composing the first prompt of a session). Otherwise the project is `""`.
- `agent` minute: it goes to the project of the latest event, at or before the end of the minute and at most 10 minutes before its start, of any turn busy in it (ties: the smaller name), or to the last 5 minutes' project.

**Cuts**
- A cut sets the `you` rows of each minute the cut span touches to 0 credit, whatever their project, and adds the cut code. The label stays `you`. A minute with a human heartbeat keeps its credit: the person was editing, whatever the agent was doing.
- A turn's cut span runs from its prompt to its end, but at most 25 minutes. Every turn rule (loop, rubber_stamp, automated_session) is decided at the prompt from the 30 minutes before it, so its effect stays within the 60 minutes of context.
- `rubber_stamp`: a stamp is a prompt that meets all of these:
  - the session's event just before it is a `turn_end`, 0 to 15 s earlier;
  - it is 1 to 25 characters long (an unknown length of 0 is never a stamp);
  - no human heartbeat came in between.

  Three consecutive prompts of a session that are all stamps, the first and third at most 30 minutes apart, make a streak; the turns of the second and third are cut. A longer run cuts every stamp after the first. The second stamp is cut only once the third arrives, and then only from 24 minutes before the third, so the rule never changes a final minute.
- `loop`: a prompt is cut when its session had at least 2 earlier prompts with the same keyed hash in the 30 minutes before it, inclusive, with no human heartbeat between them and it. This catches the 3rd and later repeats of a prompt retried without editing. An empty hash never matches, and prompts of 1 to 25 characters ("continue", "yes") are left to `rubber_stamp`.

**Flags**
- `automated_session`: a `prompt_submit` with no OS input instant (sample time minus idle) in the 2 minutes before it (up to 2 s after it, for sampler delay). Only the activity samples taken within 2 minutes of the prompt are read, and prompts with none are not judged, because missing data isn't evidence. A sample taken later can still report input from before the prompt (a sensor catching up after a stall), but it isn't read: it may arrive after the prompt's minute is final. The flag goes on all rows of the turn's cut span (above).
- `scripted_input`: 30 consecutive intervals with a population standard deviation under 0.2 s. It is checked on two streams:
  - heartbeat intervals between 1 s and 115 s. Faster than the WakaTime 2-minute throttle, so steady typing, which arrives at ~120 s intervals, can't trigger it, and bulk duplicates (0 s) don't either.
  - OS input instants that a later sample saw at least 2 s of quiet after, with intervals between 2 s and 10 min. Observations of the same instant within 0.5 s are merged. Continuous typing is seen at the sampler's own 5 s rhythm and never has 2 s of quiet, so it is skipped; a jiggler's fixed period is caught.

  The flag goes on all rows of the minutes from the first to the last event of the run, reaching back at most 24 minutes from the run's last event.
- `otel_mismatch`: computed for each session that has active-time data. The harness defaults to `claude-code`.
  - The session's minutes are the window's minutes in which the session had an AI event or a turn, and in which the dominant focus was a terminal, AI app or unknown. Time spent in the editor while the agent works isn't compared, because Claude Code can't see it.
  - Credited = the credited seconds of those minutes.
  - Active = the session's `UserSecs + CLISecs`, prorated by each datapoint's overlap with the window. Claude Code counts `user` for keyboard interactions and `cli` for tool execution and AI responses, so a person watching the agent work is covered by `cli`; comparing `user` alone would flag every honest "prompt, then watch" session.
  - The flag goes on the `you` rows of those minutes when active < 50 % of credited and credited − active > 10 min: the hooks claim far more harness time than Claude Code itself measured.
- `long_day`: a running credited total per local day (`Input.TZ`, UTC if nil), counted within the window. Credited rows are flagged from the minute the total passes 12 h.
- `otel_mismatch`, `long_day` and `scripted_input` depend on the window: a jiggler's input run can last 5 hours, longer than the agent's 90 minutes of data. The server recomputes them over whole days, and when it compares the agent's minutes with its own re-run it treats them as advisory. Every other flag, and every label, credit and cut, is final under the context rule above. Two property tests check this on every scenario, with extra activity samples arriving just past the 25-minute lookahead and an hour after each prompt: one replays it as the agent sees it, minute by minute; the other re-runs it the way the server does, with windows ending at every minute. A third generates random scenarios (stalls, late samples, long turns and waits, and the event shapes of every harness's hooks: Copilot's tool ends without starts, Cursor's tool calls reported only when they end, some after more than 25 minutes, OpenCode's answered asks, session starts mid-turn) and checks that every minute computed from only its own 60 minutes before and 25 after equals the full run, and replays each as the agent sees it too. Every replay holds only the events that had arrived by its time, so an event stamped earlier than it could arrive fails them.
- `scripted_input` is the one rule that reads input instants from samples taken any time later (whether some later sample saw quiet after an instant); it is window-relative and advisory for that reason too.

**Determinism and cost**
- The engine sorts copies of every input with a total order and drops exact duplicates. Maps are used only for lookups, never to order output. `time.Now` is never read.
- A 2-hour window with 60 minutes of context takes about 7 to 12 ms on a shared 4-vCPU server (AMD EPYC) (`BenchmarkCompute2h`). The input has activity every 5 s, heartbeats every ~30 s, an agent turn every 5 minutes with a tool call every ~10 s, and OTel points. Memory is linear in events plus minutes.

**Changes after review (still rules v1, before any release)**

These changed the meaning of the rules above, not only the code, and need the owner's sign-off: presence now follows the "idle < 60 s at a sample" wording (about one more credited minute after each pause); browser and unknown focus need the person's own signals, not an agent's; input with no project and no active session isn't credited; minutes are split by the person's own signals; the loop rule ignores short prompts and repeats with editing between them; turn cuts reach 25 minutes after the prompt and spare minutes with human editing; `otel_mismatch` compares against user + cli time; a file switch is no longer movement on low-signal devices.

Later fixes from a second review, also still rules v1:
- Project names are normalised as the server does before projects are counted. Before, `app` and ` app ` in one minute became two 30 s rows; the server merged them into one name, rejected the second as a duplicate and stored 30 s. `<<LAST_PROJECT>>` now means no project instead of a project of that name.
- `automated_session` reads only the activity samples within 2 minutes of the prompt. Before, a sample an hour later that reported input at the prompt (after a sensor stall) removed the flag from a minute that was already final.
- An unanswered `permission_ask` outside any turn opens a turn that is waiting, as a `tool_start` opens a running one. Before, a wait whose asks repeated for over an hour was only known to callers that held the turn's prompt, so the answer (a signal of the person) and the busy time after it differed between a full run and the agent's 90 minutes of data.
- A `tool_end` or an answered `permission_ask` outside any turn starts an inferred turn, as a `tool_start` and an unanswered ask already did, and `session_start` no longer keeps a turn running. Before, a Copilot turn (tool ends only) that ran for over an hour was `agent` in a full run but `idle` for the agent's 90 minutes of data, once its prompt had dropped out; a stream of `session_start` events after a prompt had the same effect. A stray `tool_end` after a `turn_end` now makes its minute agent-busy, and a `session_start` during a permission wait no longer counts as the person's answer.
- A minute is split among at most 8 projects, the busiest ones. Before, a minute with signals in 9 or more projects had 9 or more rows; the server stores at most 8 per device-minute, so it rejected the rest and their seconds were lost (54 s instead of 60 s for 9 equal projects).

Fixes from a third review, also still rules v1:
- Cursor's tool calls are recorded as one `tool_end` when the call ends, with no `tool_start` back-dated by its duration. Before, the back-dated start arrived only when the call ended, up to 6 hours later, and could change minutes that were already final: with the same long prompt sent at −4, −2 and 0 minutes and a call from +8 to +48, minute +2 was final at 60 s, then the start at +8 extended the third prompt's turn through +2, and a re-run cut it as `loop` to 0 s, so the agent's and the server's results disagreed. Now a call's session doesn't make its quiet minutes agent-busy, and the prompt before it doesn't cut them; the person keeps their credit.
- Every `permission_ask` carrying a `Decision` (OpenCode's reply) is the person's signal. Before, a reply counted only when the engine had seen the ask it answered and the wait was still open; a reply whose ask was before the caller's data, or had timed out, opened an inferred turn but wasn't a signal, so with fresh input and browser or unknown focus the minute was `agent` with 0 credit instead of `you`.
