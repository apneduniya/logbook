# 0012: Struggle engine v1: bounded lookahead, sparse output, window-relative flags

- Status: accepted
- Date: 2026-11-20

## Context

ADR 0010 has the agent run `struggle.Compute` every minute over a sliding window. It uploads the labelled minutes, which become final once they are 30 minutes old. The server later re-runs the same rules over whole days of submitted evidence and flags any difference. That only works if:
- a minute's result settles before the agent stops rewriting it;
- the agent's view and the server's view agree.

Some rules in `docs/STRUGGLE-RULES.md` naturally look ahead. A rubber-stamp streak is only known at its third stamp, and a turn that times out is only known 10 minutes later. Others aggregate over a span, such as OTel time per session and credited hours per day.

## Decision

1. **Bounded lookahead and look-back.**
   - No rule looks more than 30 minutes back or ahead of a minute.
   - The rubber-stamp streak cuts the second stamp's turn when the third arrives, but only back to 30 minutes before the third.
   - A timed-out turn ends at its last event, which is at most 10 minutes of lookahead.
   - Callers pass at least 30 minutes of context before the window. The server also passes 30 minutes after the window when it re-runs closed history.
   - Given that context, a minute's label, credit, cuts and window-independent flags don't depend on where the window starts or ends, once the minute is 30 minutes older than the end of the data. A scripted-input flag reaches back at most 30 minutes, and a turn's project is that of its first event. `TestFinalMinutesMatchAgentReplay` checks this on every golden scenario.
2. **Sparse output.** Idle minutes with no project aren't emitted. That means nothing happened in the last 5 minutes, and a missing minute means idle. This keeps `credited_minutes` from storing a row for every minute a laptop is on.
3. **Window-relative flags.** `otel_mismatch` and `long_day` are computed within the window, and the server recomputes them over whole days. So when the server compares agent-uploaded minutes with its own re-run, it compares `label`, `credited` and `cuts`, and treats flags as advisory. The other flags are window-independent, as are labels, credits and cuts.
4. **Cuts remove the whole minute.** A `loop` or `rubber_stamp` cut zeroes every `you` row of the minutes the turn touches, whatever their project, because credit is the person's time.
5. **Presence from input instants.** An activity sample at `t` with idle `i` means input at `t − i`. A minute is present when one of those instants falls inside it. This is stricter than "idle < 60 s at a sample in the minute" but means the same thing, and it is what the agent's 5-second samples can support.

## Consequences

- The agent's final minutes match a server re-run, except for the two window-relative flags.
- Server code reading `credited_minutes` must treat absent minutes as idle.
- Rules v2 must keep within the 30-minute lookahead, or raise the finality age in the same version.
