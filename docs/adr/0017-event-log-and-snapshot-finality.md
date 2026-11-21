# 0017: Event log and snapshot finality

- Status: accepted
- Date: 2026-11-21
- Supersedes: ADR 0016; the "Which events a day covers", "Minutes" (repo mapping groups, finality) and "Evidence days" parts of ADR 0013; the step-5 cutoff note in ADR 0014

## Context

A credited minute must be reproducible: the agent's stored value, the server's copy and a later re-run of the engine (step 5) must agree. ADR 0013 and ADR 0016 got there with clocks. They used an arrival time on every event, a cutoff per run, a finality watermark, a settle period, repo mapping groups keyed by time, and evidence days defined by event time. Four rounds of cross-model review each found new ways for those clocks to disagree:

- a row stamped before a reader's cutoff but not yet committed;
- clock rollback reopening final minutes and forgetting cutoffs;
- suspend hiding from the monotonic clock;
- a long spool drain letting a provisional value freeze on the server;
- repo mappings using +30 minutes while finality used +29;
- events arriving after their day was sealed still changing the next day's credit;
- a cache generation read outside the refresh it guarded.

Every fix added a clock rule, and every rule had new edges. The root problem: finality was *inferred from time* when it can be *recorded*.

## Decision

**1. One append-only event log.** Every raw event the engine can read gets a sequence number from a single counter when it is stored. That covers heartbeats, AI events, activity samples, git events, Claude Code active time and repo mapping rows. The counter (`kv` `log.seq`) is incremented inside the inserting transaction. SQLite serialises writers, so the committed rows are always exactly the sequence numbers `1..H` that are still present. An ignored duplicate (heartbeat, commit) takes no number. Logged rows are never updated. `arrived_at`, `credit_cutoffs`, `credited_minutes.cutoff`, `evidence_days.cutoffs` and `evidence_days.active_time` are removed.

**2. A snapshot is one number.** A crediting run opens one read transaction and reads `H`, the log's high-water mark, then everything it uses, all inside that transaction. It reads events with `seq ≤ H` whose time is in its history window. It reads the repo mapping as the latest row per path with `seq ≤ H`. A write that hasn't committed is entirely outside the snapshot. The run's *computation key* is `(T, H)`:
- `T` is the end of its window, a whole minute on the agent's clock;
- the engine input is the window `[T − 60 min, T)`, with 60 minutes of history before it (`struggle.ContextBefore`), `DataEnd = T`, and the log at `H`.

A run is a pure function of the log and `(T, H)`. There is one repo mapping per run, so the mapping groups of ADR 0013 go away.

**3. Every stored minute records the key that produced it.** `credited_minutes` gets `computed_at` (`T`) and `snapshot` (`H`). A device-minute is one unit: all of its rows carry the same key, change together and are uploaded in one request.
- When a run's result for an open minute differs from the stored rows, all of that minute's rows are replaced with the new rows and the run's key.
- When the result is the same, the stored rows and their older key are kept. That key still reproduces the same value, and keeping it means unchanged minutes are never re-sent.

**4. The server stores the key and returns it.** Each uploaded item carries `computed_at` and `snapshot`, and the server stores them with the row. A changed key counts as a rewrite under the existing 30-minute finality. The offline catch-up exception of ADR 0014 (a final minute first uploaded after it was final takes more projects for 5 minutes) takes only projects carrying the key of the minute's stored rows, so a device-minute never holds rows of two computations. When the server refuses a write because the minute is final, the rejection includes the rows it holds for that device-minute (`"stored": [{project, label, credited, cuts, flags, computed_at, snapshot}]`). That is the server's own answer, so a lost response can't leave the agent guessing which value won. Sending exactly the stored row again, key included, is still accepted as a repeat.

**5. Finality is a recorded state, not a clock comparison.** Each local minute is `open` or `final`:
- **Open** minutes are recomputed by every run whose window covers them.
- **Final** minutes are never recomputed, whatever the clock does later. A final minute that is not yet acknowledged keeps being uploaded until the server accepts it or answers with its stored rows.
- The agent makes a minute final at its first *settled* run with `T ≥ m + 29 min` (`FinalLead`).
- If the server refuses a minute as final and returns its stored rows, the agent replaces its rows with exactly those rows (values and key) and marks the minute final and acknowledged. Every value the server holds came from this agent with its key, so it can be replayed. A returned row without a key (stored before this ADR) is adopted, counted (`crediting.unkeyed`) and shown in status.
- **The value the server accepted is the truth.** The agent never shows or seals a final value the server doesn't hold.

**6. Settling is about fairness only.** After a start, a wake or a long gap between runs, the agent waits before it makes anything final. It waits 15 minutes (`Settle`) and until the startup spool drain has finished, so that late honest events (from the spool, the wakatime-cli offline queue or a delayed OTLP export) still count. A wake is detected by a wall-clock gap or a monotonic gap of more than two intervals plus a minute.
- While unsettled, minutes the server would freeze before the agent settles are held back, as in ADR 0016.
- None of this affects whether a replay agrees. If settling goes wrong, the worst case is that an honest late event isn't credited. The agent and the server never disagree because of it.

**7. Evidence blocks seal log ranges.** The server API is unchanged: one signed record per UTC day `D`, chained, `day` in the chain hash. Its content changes.
- The record for `D` covers every logged row with `seq` in `(H_prev, H_D]`. `H_prev` is where the previous block of this device's chain ended. `H_D` is the log's high-water mark read in the same transaction that seals.
- A block can hold events of any time. A late event for an earlier day goes into whichever block is open when it arrives, so every row is sealed exactly once.
- A device's first block starts at the oldest retained row. A block is read and hashed in chunks of sequence numbers (`evidence.Hasher`), a bounded number per run, with the progress kept between runs, so a large first block (up to 90 days of rows) needs neither the whole block in memory nor one run's time. This is sound because rows in an unsealed range are never updated or deleted; `H_D` is the high-water mark of the snapshot that reads the last chunk.
- Every key `(T, H)` of a minute in day `D` has `H ≤ H_D`. A minute of `D` is final and acknowledged before `D` is sealed, and its key was taken before that. So every final minute can be replayed from sealed blocks.
- Sealing conditions are unchanged from ADR 0013. The server refuses first-time minutes in a sealed day, so a day is sealed only when:
  - it has ended plus a grace hour on both clocks;
  - the system clock and the server's `server_time` agree to within 5 minutes;
  - every stored minute of the day is final and acknowledged;
  - this run's uploads went through.
  
  The `sealed_through` guard stays: no minute before it is computed or stored.
- `evidence_days` stores `seq_from` and `seq_to`. A range with no rows is skipped (a gap).

**8. Canonical encoding** (`pkg/evidence`). One `{"s","t","type","v"}` line per logged row, `\n`-terminated, sorted by `s` (the sequence number, unique, so the sort is total).
- `type` is `activity`, `active_time`, `ai`, `git`, `heartbeat` or `repo` (new: `{"path","project"}`).
- `v` is the stored row as before, without derived fields. The `"a"` arrival field is removed.
- The golden file and pinned content hash change. This is still pre-release, so the chain domain `logbook-evidence-v1` stays.
- Replaying a block (`crediting.ReplayBlock`) re-hashes the rows in its range. Replaying a minute (`crediting.ReplayMinute`) runs the engine at the minute's key over rows with `seq ≤ H`. Replays compare label, project, credit and cuts. Flags are left out, because the time zone and the active-time derivation feed only flags.

**9. Retention never breaks the log.** Pruning (90 days) and the row cap delete only rows already sealed by the current device chain. If unsealed rows reach the cap, new activity samples are dropped and counted rather than deleting unsealed history. Heartbeats, AI events, git events, active time and repo rows are always kept. The event cache refills when the deletion generation changes, and reads the generation inside the same snapshot transaction as its rows.

**10. Legacy paths go.** Days sealed before this change, the arrival-based replay (`ReplayDayCredit`, `ReplayMinutes` with cutoffs) and the mapping-group code are deleted, not kept behind flags. This is pre-release data on development devices only. Local migration 0007 backfills `seq` for existing rows in `(arrived_at, table, id)` order. It marks evidence days sealed before it as legacy (not replayable). Those devices start a new block range at the next block.

## Consequences

- Agent, server and replay agree by construction. A minute's value is the deterministic function of `(T, H)` that produced it, and the server stores that key. The round-4 counterexamples become regression tests:
  - uncommitted writes;
  - clock rollback;
  - a frozen server value;
  - suspend;
  - a long drain;
  - a repo mapping change at +29/+30;
  - a post-seal arrival;
  - a cache generation race;
  - a legacy replay.
- The clock now decides only *when* things happen (window position, finality timing, settle, sealing), never *which data* a result used.
- One new server migration (`00004`): `credited_minutes.computed_at` and `snapshot`, nullable for pre-existing rows and required on upload. Rejection bodies gain `stored`. Agents built before this ADR are rejected for the missing fields, which is acceptable before release.
- A step-5 bundle carries the blocks covering each submitted minute's key (back to `T − 120 min`) and the minute rows with their keys. The reviewer's server re-runs each minute at its key and re-hashes each block.
- The first block of a device hashes up to 90 days of retained rows once, in chunks over as many runs as it needs. That is a hash over local data, and nothing leaves the device.
