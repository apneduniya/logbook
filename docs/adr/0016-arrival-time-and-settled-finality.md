# 0016: Arrival time and settled finality

- Status: accepted
- Date: 2026-11-21

## Context

A minute's credit becomes final 30 minutes after it starts: the server takes no rewrite after that (ADR 0010, ADR 0014), and the agent froze a minute once it was 30 minutes old (ADR 0013). ADR 0012 makes a minute depend only on events from 60 minutes before it to 25 minutes after it, so a minute computed at 29 minutes is final *if every event about it has arrived by then*.

They don't always. Events reach the agent late when it was down: hooks append to the spool while the daemon is stopped and the daemon drains it at start; wakatime-cli keeps heartbeats in its offline queue and flushes them when the agent answers again. A cross-model review reproduced two disagreements with the real agent pipeline, server and Postgres:

- **Spool drain race.** The same long prompt at −4, −2 and 0 minutes, the agent stopped at +1, the turn's tool calls spooled at +8 and +16, the agent started at +47. Spool draining ran concurrently with crediting, so the catch-up run at +48 could run first: it stored minute +2 at 60 s and uploaded it as final. A replay after the drain extends the third prompt's turn through +2 and cuts it as `loop`, 0 s. The agent's minute and the server's re-run disagree for good.
- **Offline queue.** An editor heartbeat for +2 arriving at +48 (wakatime-cli flushing after the agent was down): the catch-up run had frozen +2 as idle, a replay credits 60 s. Honest credit is lost, and the two disagree again.

The second kind is not limited to restarts: any event can arrive after the minute it is about is final. Something has to decide which events a final minute counts, the same way on the agent and in every replay.

## Decision

**1. Every raw event records its arrival.** `heartbeats`, `ai_events` (spool-drained ones included), `activity_samples`, `git_events` and `active_time` get `arrived_at`: the agent's wall clock in unix ms, taken inside the inserting transaction while it holds SQLite's write lock (so a row stamped before a reader's cutoff is committed before that reader starts). A resent heartbeat or commit keeps its first arrival. Local migration 0006 adds the column; rows stored before it have no known arrival and get their own event time (`to_ms` for active time), the earliest they can have arrived.

**2. Arrival cutoffs.** Each crediting run has a cutoff: the end of its window, the whole minute a few seconds before the run. The engine's input is the events in the window's history that arrived by the cutoff; one that arrives later counts from the next run. A run is therefore a pure function of the raw events, the repo mapping journal and its cutoff: the window is `[cutoff − 60 min, cutoff)` with 60 minutes of history before it (the engine's whole look-back, `struggle.ContextBefore`; it was 30) and `DataEnd` at the cutoff. Every stored minute row records the cutoff of the run that last wrote or confirmed it (`credited_minutes.cutoff`).

**3. Settled finality.** Finality is a watermark, `crediting.final_through`: every minute before it is final and frozen; it only moves forward.

- While the agent has been up for a while, a minute `m` is made final by the first run at or after `m + 29 min` (`FinalLead`), with that run's cutoff. The server still takes that write (its limit is 30 minutes), and 29 minutes is past the engine's 25-minute lookahead.
- After a start, or a gap in runs longer than two intervals plus a minute (the machine slept), no minute becomes final until the agent has been up for the settle period (`Settle`, 15 minutes) **and** the startup spool drain has finished. The daemon closes a `Drained` channel after its first drain attempt; the crediting runner waits for it, at most 2 minutes, before its first run, and treats the agent as unsettled until it is closed.
- While settling, minutes are still computed and stored for the status page. Minutes that are open on the server and will be finalized after the agent settles are uploaded as usual. Minutes the server would freeze before the agent can finalize them (`m + 29 min` earlier than the settle instant) are **held**: not uploaded until final. Uploading them would make a provisional value final on the server.
- A minute the server has already frozen (a row it accepted and the minute is 30 minutes old) is frozen locally too, with the cutoff of the run that wrote it, even before the watermark reaches it. So after a restart the minutes uploaded open before the stop keep exactly the value the server holds.
- When the watermark moves, each newly final minute gets its cutoff: the run's own for a minute it rewrote, the stored rows' for a frozen minute or one outside the run's window, and none (zero, "never computed") for a minute with no rows outside it. Only cutoffs other than `minute + 29 min` are recorded (`credit_cutoffs`, merged ranges); a day without restarts records none.
- If the system clock is set back before the watermark, the watermark follows it back to the clock's own limit (no earlier than the run's window) and the cutoffs recorded after it are forgotten, as the derived finality did before. Only a clock change can do this.

**4. Late events are kept, counted and ignored for final minutes.** An event that arrives after its minute's cutoff never changes that minute's credit. It is stored and sealed with its day like any event (it is a fact), and it can still count for minutes that aren't final yet. The agent counts the events whose own minute was already final when they arrived and shows "late events ignored: N, latest …" in status (`crediting.late_ignored`, `crediting.late_latest`).

**5. Replays use the same cutoffs.** `crediting.ReplayMinutes` recomputes final minutes from raw events, the repo journal and the cutoffs: for each distinct cutoff it runs exactly the agent's window for that cutoff over the events that had arrived by then, and keeps the minutes finalized with it. `crediting.ReplayDayCredit` does this for a sealed day with the cutoffs stored in `evidence_days.cutoffs` (written at sealing, JSON `[{"from","to","cutoff"}]`, `[]` when all are the default; NULL for days sealed before migration 0006, which can't be replayed). A day is sealed only once the watermark has passed its end, so its cutoffs are complete. Claude Code active time isn't replayed: it feeds only the advisory `otel_mismatch` flag. A replay is compared on label, project, credit and cuts.

**6. The arrival is part of the canonical encoding.** A replay needs it to leave out exactly the events the agent left out, and the server must be able to trust it, so it is covered by the day's content hash and signature. Each line of `pkg/evidence.Encode` gains `"a"` (RFC 3339 UTC, after `"t"`); active-time lines have none (no cutoff applies to them). Lines are sorted by time, type, the bytes of `v`, then the arrival, so the same event stored twice with different arrivals encodes deterministically. `evidence.DayEvents` holds `evidence.Received[T]` (event plus arrival), and a raw event without an arrival is an error. The golden file and pinned hash change. This is a format change made before any release, so the chain domain (`logbook-evidence-v1`, `chain.go`) is unchanged; after a release it would need a new domain. The cutoffs are not hashed: like the repo mapping journal they travel in a step-5 bundle next to the day's events. They can only leave out events the agent itself stamped as late, which it could equally have never recorded.

**7. The event cache notices deletions.** The crediting runner's in-memory window (ADR 0013) refilled itself only when row ids went back, so rows deleted by pruning or by the row cap stayed in memory while ids kept growing: with a cap of 2 rows, a third sample left 3 cached against 2 stored, and a kept presence sample credited 60 s that a cold read left idle. The local database now keeps a deletion generation (`kv` `events.generation`), bumped in the same transaction as any prune or cap trim that deletes raw event rows; the cache reads it first and refills when it changed.

## Consequences

- The agent's stored final minutes, the server's copy and a replay of the raw events with the stored cutoffs agree, whatever order events arrive in (`TestFinalMinutesNeverChangeAndReplay`, the e2e tests in `internal/e2e/late_test.go`).
- Events that arrive within the settle period after a restart count; later ones are kept and counted but never change a final minute.
- After every start or wake the agent makes nothing final, and uploads no minute that would become final on the server, for 15 minutes (longer if the spool drain is slow). Live minutes still go up within a minute.
- Every run reads twice the history (2 hours instead of 90 minutes); warm runs still read only new rows. At the row cap every insert trims and moves the generation, so each run reads its window cold (about 17 ms on the dev VPS).
- A minute cut short by a stop (computed only while open, then frozen by the server) keeps that value, with its early cutoff; the replay reproduces it.
- Days sealed before migration 0006 hash without arrivals and can't be re-hashed or replayed; this is pre-release data only.
