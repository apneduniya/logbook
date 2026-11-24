# 0020: Submissions, the server re-check and the review link

- Status: accepted
- Date: 2026-11-25
- Replaces: the "Submission (step 5)" part of ADR 0010 and the bundle note at the end of ADR 0017

## Context

The bounty is won when programs approve hours from Logbook into the Unified YSWS DB (1,000 weighted projects, about 10,000 hours). What stands between us and that is review:
- Stardance approved about 27,600 of 123,400 submitted hours, by hand. Reviewers need to see quickly what an hour was spent on, with time linked to commits.
- Reviewers won't watch footage or read 100 hours of anything.
- The common objection to any client-side tracker is that its numbers are self-reported. Our answer has to be visible on the page: the server re-runs the rules on the device's signed logs.
- Programs record hours in their own Airtable, with a "Justification - Alternate Tracking Method" field, and HQ removes rows whose evidence can't be found later. A link must keep working after review.

## Decision

### What a submission is

A submission is one project, one UTC date range of whole days, and optionally one program (ADR 0021). It's made by the user's agent, because only the agent holds the raw events.

**Which minutes:** every credited-minute row the server holds for that user, project and range, from every device, frozen at the moment of submission, together with the project's **capped** seconds for each of those minutes. When devices together credit more than 60 s in a minute, the cap (ADR 0014, `credited_project_minutes`) shares the minute across all projects and devices, so the project's value depends on rows outside the submission; freezing the capped value keeps that. The packet's totals and every decision use the frozen capped values; the device rows are what the re-check replays.

**Who can submit:** a user whose linked Hackatime account is banned (`red`, unmasked, as `hackatime.TrustLevel.Banned` defines) gets `403`. The trust level is re-read from Hackatime when it's more than 24 hours old; if Hackatime can't be reached, the last known level decides. A user with no Hackatime link can submit; the review page says "Hackatime not linked" so the program can apply its own rule.

**Range:** at most 92 days, ending no later than the last day every included device has sealed. The agent refuses a range ending today, with a plain reason: "today's evidence is sealed after midnight UTC".

### The bundle (agent → server)

`logbook submit <project> --from D1 --to D2 [--program <slug>]`:
1. Asks the server for the minute rows it holds for the range (`GET /api/v1/me/submissions/plan`), with each row's device and key `(T, H)`.
2. For each of this device's minutes, takes the window its run read, minutes `[T − 120 min, T)` (the 60-minute window and its 60 minutes of context; the run's data ends at `T`), and merges the windows into ranges.
3. Builds one proof (ADR 0019) per sealed block that matters: it opens every minute bucket in those ranges with their edges (completeness), for each block whose signed time span overlaps a range and whose `seq_from` is below the largest `H`, and the repo bucket of each block holding some minute's `H`. If a bucket it must open has lost rows to pruning, the minutes that need it are sent without evidence and show as unbacked.
4. Prints what will be uploaded, then asks for confirmation (`--yes` skips it):
   - minutes, days and blocks included;
   - how many rows from **other projects** are inside the replay windows (their file names and AI session metadata are revealed too, because the engine needs them to split a minute), listed by project name, and the repo folders in the opened repo buckets;
   - what is never included: prompts, code, keystrokes, file contents (the agent never has them).
   `--preview FILE` writes the bundle to a file instead, for inspection.
5. Uploads it gzip-compressed (`POST /api/v1/submissions`).

Minutes from other devices are part of the submission but have no evidence until that device adds its part (`logbook submit --add <submission-id>`, run there). The review link shows them as "not yet backed by evidence".

### The server re-check

A River job verifies each bundle part, then builds the review packet:
1. **Blocks:** every proof's root equals the stored `content_hash` for that device and day, whose signature and chain were checked when it was uploaded. Every block that must show completeness for a minute's window (by its signed time span) does; otherwise that minute is unbacked.
2. **Minutes:** for each distinct key `(T, H)`, it runs `pkg/struggle` with the rules version the minute recorded, over the opened rows with `seq ≤ H` in the window ending at `T` and the repo rows with `seq ≤ H` from the repo bucket of `H`'s block (reduced to the highest `seq` per path), and compares label, project, credit and cuts with the frozen rows (flags are advisory, as in ADR 0017). Minutes sharing a key are replayed in one run, so 100 hours take seconds.
3. The code that turns evidence rows into engine input moves from `internal/agent/crediting` to a new package, `pkg/replay`. The agent's own replay and the server use the same function, so they can't drift.

Each minute ends up `verified`, `mismatch` (with the server's result next to the device's), or `unbacked` (no evidence part yet, or its block was never sealed). The packet counts each.

A mismatch is a flag for the reviewer, not an automatic rejection: the reviewer decides.

### The review packet

Built once per verification and stored with the submission, so the review link is fast and doesn't re-read raw rows.

1. **Headline:** counted hours; time at the keyboard for the same project and days (the Hackatime-equivalent raw figure); and what the handbook's flat rule would give (raw time with `ai coding` counted at one third), so the reviewer sees where Logbook falls against the rule they already use.
2. **Evidence status, in one line:** "All 2,431 minutes re-computed on our server from signed device logs" or the counts of mismatched and unbacked minutes. Devices and evidence chain gaps (days the agent was off).
3. **What the hours are made of:** counted minutes split by what the person was doing, from the revealed rows of that minute: *editing* (their own editor heartbeat), *working with AI* (a prompt, permission answer or a turn they were watching), *other* (reading, testing, terminal). Display only; it never changes credit.
4. **What was cut and why:** minutes per cut code with the rule in plain words, and each cut's time ranges. Flags the same way.
5. **Days:** one strip per day, minute by minute (counted / AI ran alone / idle / cut), with commits as markers (short hash, files changed, lines added and removed; linked to GitHub when the project has a `repo_url`).
6. **AI sessions:** harness, model, prompts sent, turns, the person's counted minutes and the agent's alone minutes in it.
7. **Justification text** for Airtable: one paragraph with the counts above, the rules version and the review link. It's built when the packet is served, not stored, because it contains the link.

The packet never shows other projects, file paths outside the project, or anything the bundle didn't need. Fraud Squad can read the revealed rows themselves through the admin API (ADR 0022).

### The review link

- `https://logbook.boogles.dev/r/<token>`: a 128-bit random token. It's looked up by its SHA-256 hash and also stored AES-256-GCM encrypted (the server key, the submission id as associated data), so the submitter's dashboard can show the link again. Anyone with the link can read the packet, without signing in, so it works in Airtable and in HQ spot checks months later.
- The submitter sees and copies it from `/submissions`. They can withdraw a submission until a decision is recorded. After that it can't be withdrawn: the program and HQ rely on it.
- Pages are `noindex` and send `Referrer-Policy: no-referrer`.

### Decisions

- A member of the submission's program (ADR 0021) can **approve**, **adjust** (approved hours, which can only be lower than counted, plus a reason) or **reject** (a reason). The latest decision is shown on the packet with who and when. Every decision goes into `audit_log`.
- The submitter can never decide on their own submission.
- The review page shows the submitter's Hackatime trust level (masked as Hackatime masks it publicly) at submission, stored, and now, read when the page is served, with when it was last checked. Recording a decision first refreshes the trust level if it's more than 24 hours old (the last known level decides if Hackatime can't be reached; `red` is sticky, as `TrustRefresher` already keeps it). Approving or adjusting while the submitter is banned is refused (`409`); rejecting is allowed.
- A submission with no program has no decisions. Its link is evidence only.

### Checking it without us

`logbook verify <bundle>` (a subcommand of the agent binary, using only `pkg/` code) takes the verification bundle that the admin API exports (ADR 0022): the parts as uploaded, the signed evidence records, the devices' public keys, the frozen minutes, and the cap inputs: every credited row, of every project and device of the user, for each submitted minute. It checks the signatures and chain, every proof and every replay, recomputes the cap, and prints the same counts as the packet. An outside auditor doesn't have to trust our server. The submission therefore freezes the cap inputs too, not just the capped result.

### Retention

- The packet and decisions are kept while the account exists.
- The revealed raw rows are kept for one year after the latest decision (or after submission, if none), then deleted by a job. The packet notes when.

## Consequences

- Reviewers get one link per submission, readable in under a minute, and a number they can paste.
- The re-check answers "it's just self report" as far as any passive tracker can: the server re-derives every counted minute from logs that were signed on the day.
- `pkg/replay` is a refactor of agent code; the agent's replay tests must pass unchanged before the server uses it.
- Bundles are stored in Postgres (`bytea`, gzip) behind a small interface. If the step 6 load test shows they cost too much there, they move to object storage with no API change.
