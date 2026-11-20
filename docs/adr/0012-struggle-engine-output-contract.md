# 0012: Struggle engine v1: bounded context, sparse output, window-relative flags

- Status: accepted
- Date: 2026-11-20

## Context

ADR 0010 has the agent run `struggle.Compute` every minute over a sliding window. It uploads the labelled minutes, which become final once they are 30 minutes old: the server rejects later rewrites. The server later re-runs the same rules over whole days of submitted evidence and flags any difference. That only works if:
- a minute's result settles before the agent stops rewriting it;
- the agent's view and the server's view agree.

Some rules in `docs/STRUGGLE-RULES.md` look ahead: a rubber-stamp streak is only known at its third stamp, and a turn that times out is only known 10 minutes later. Rules decided at a turn's prompt (loop, rubber_stamp, automated_session) apply to the whole turn, which can run for hours. Others aggregate over a span, such as OTel time per session and credited hours per day.

A first version claimed a 30-minute look-back but let a loop cut cover a turn of any length, and ended open turns at the window's end even when the server had data beyond it. Review reproduced agent/server disagreements from both.

## Decision

1. **Bounded context.** A minute's label, credit, cuts and window-independent flags depend only on events from `ContextBefore` (60 minutes) before the minute's start to `Lookahead` (25 minutes) after it. To keep that true:
   - a turn rule is decided at the prompt, from the 30 minutes before it, and covers the turn for at most 25 minutes after the prompt;
   - a turn's agent-minute project comes from its recent events, never from its first event, which the caller's data may cut off;
   - the rubber-stamp streak cuts the second stamp's turn when the third arrives, but only from 24 minutes before the third;
   - a timed-out turn ends at its last event, which is at most 10 minutes of lookahead;
   - movement on low-signal devices compares with the same file's position within 30 minutes;
   - `automated_session` judges a prompt only from activity samples taken within 2 minutes of it, though a later sample can report earlier input;
   - the end of the data is the latest of `Window.To`, `Input.DataEnd` and every event. The server sets `DataEnd` when it passes data after `To`, so a turn that went quiet just before `To` ends where the agent, which saw the quiet, ended it.
2. **Callers.** The agent computes the last 60 minutes from the last 90 minutes of events; its rewrites of a minute made when the minute is 25 to 30 minutes old are final. The server passes 60 minutes before `From` and 30 minutes after `To`, and sets `DataEnd`. `TestFinalMinutesMatchAgentReplay` and `TestServerSubWindowMatchesFullRun` check both on every golden scenario, with late samples added past the lookahead, and `TestBoundedContext` checks the context rule itself on random scenarios.
3. **Sparse output.** Idle minutes with no project aren't emitted. That means nothing happened in the last 5 minutes, and a missing minute means idle. This keeps `credited_minutes` from storing a row for every minute a laptop is on.
4. **Window-relative flags.** `otel_mismatch`, `long_day` and `scripted_input` are computed within the window (a jiggler's 30-interval run can last hours), and the server recomputes them over whole days. So when the server compares agent-uploaded minutes with its own re-run, it compares `label`, `project`, `credited` and `cuts`, and treats those flags as advisory.
5. **Cuts remove the person's time in the turn.** A `loop` or `rubber_stamp` cut zeroes the `you` rows of the minutes the turn's cut span touches, whatever their project, because credit is the person's time. A minute with a human (non-AI) heartbeat keeps its credit: the person was editing.
6. **Output fits the server.** Every row the engine emits is one the server stores (ADR 0014): project names are already normalised with `pkg/projectname`, so no two rows of a minute collapse into one, and a minute has at most `MaxProjectsPerMinute` (8) rows, the busiest projects sharing its 60 s. The server's own limit is defined as the engine's constant, and a contract test in `internal/server/credited` runs engine output through `Plan` and checks that all its credit is stored.
7. **Presence as the rule words it.** A minute is present when a sample in it reports OS idle time under 60 s. An earlier version counted only input instants inside the minute; that dropped about one minute after every pause and didn't match the rule.

## Consequences

- The agent's final minutes match a server re-run, except for the window-relative flags.
- A loop or rubber-stamp turn that runs longer than 25 minutes is cut only for its first 25 minutes. Long unattended turns are still `agent_alone` when the person isn't engaged.
- Server code reading `credited_minutes` must treat absent minutes as idle, and must set `Input.DataEnd` when re-running closed history.
- Rules v2 must keep within `ContextBefore` and `Lookahead`, or change them and the agent's query range in the same version.
