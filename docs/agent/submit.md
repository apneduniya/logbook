# Submitting hours and checking a submission

The agent's side of ADR 0020: `logbook submit` uploads one device's evidence for a project's minutes, and `logbook verify` checks a whole submission offline. The server's side, the wire types and the review packet are in [docs/api/submissions.md](../api/submissions.md).

| Package | Owns |
|---|---|
| `internal/agent/submit` | Which of this device's minutes a part lists and why the others can't be backed, the summary shown before uploading, the confirmation, refusing ranges that can't be submitted yet |
| `internal/agent/audit` | `logbook verify`: reading a bundle and the step-by-step report. Imports only `pkg/` (a test enforces it) |
| `internal/agent/serverapi` | `Plan`, `Submit`, `AddPart`: the calls, gzip, timeouts and error mapping |
| `internal/agent/crediting` | `BuildProofs` (see [crediting.md](crediting.md#proofs)) |
| `pkg/submission` | The wire types, the verifier, the cap and `CheckBundle` |

## `logbook submit`

```
logbook submit <project> --from YYYY-MM-DD --to YYYY-MM-DD [--program <slug>] [--yes] [--preview FILE] [--add <submission-id>]
```

The project may come before or after the flags. Days are UTC and inclusive, at most 92.

1. **Checks before anything is sent.** The agent must be signed in, have a local log and a registered, unrevoked device for the configured server. A range ending today or later is refused: "today's evidence is sealed after midnight UTC; end it on <yesterday> or earlier". Without `--yes` or `--preview`, stdin must be a terminal (a character device), or submit refuses before calling the server: there would be no way to confirm.
2. **Plan.** `GET /api/v1/me/submissions/plan?project=…&from=…&to=…&device=<this device>`. The server's `400` for a range a device hasn't sealed is shown with its reason. The agent also refuses when the plan names a device whose `sealed_through` is before `--to` (or `null`), naming the device. An empty plan stops with "nothing to submit".
3. **Which minutes this part lists.** For each of this device's minutes in the plan: listed when every row of it has the same computation key `(T, H)`, the minute is in `[T − 60 min, T)`, and `BuildProofs` backs the key. Otherwise it isn't listed (the server shows it unbacked), counted under one reason:
   - computed before minutes recorded how they were computed (no key);
   - its rows were computed at different keys, or its key's window doesn't hold it (listing either would make the server fail the whole part, so the agent never does);
   - its evidence can't be proven from this device's log (every key `BuildProofs` leaves unbacked).

   Only keys of minutes that pass the first checks are given to `BuildProofs`, so no window is opened for a minute the part won't list.
4. **Summary**, printed before any upload or preview:
   - minutes this device backs, the days they're on and how many have credit, and the sealed blocks that prove them;
   - this device's minutes it can't back, by reason;
   - minutes of other devices (by name): they stay "not yet backed by evidence" until each runs `logbook submit … --add <id>`;
   - every row the opened buckets reveal, by project: the submitted project's, then each **other project's** rows inside the replay windows, by name (`(no project)` for events with none), with heartbeats, AI events and git events counted separately. Events are attributed as the engine does (`struggle.RepoMap`) with the repo mapping at the highest key the part backs; git events by their own project name. Activity samples and Claude Code active-time rows belong to no project and are counted on their own;
   - the repo folders and their projects from the opened repo buckets;
   - what is never included: prompt text, code, keystrokes and file contents (the agent never records them). It also says AI events carry a prompt's length and its keyed hash, because the signed lines can't be edited without breaking the proof.
5. **Confirmation.** "Upload this to <server>? [y/N]". Only `y` or `yes` (any case, surrounding spaces ignored) uploads; anything else, or end of input, cancels with nothing uploaded. `--yes` skips the question.
6. **Upload.** The body is the JSON of `submission.Upload` (or, with `--add`, of `submission.Part` alone), checked with its `Validate` first, then gzip-compressed and sent with `Content-Encoding: gzip`. A body over 256 MiB uncompressed or 32 MiB compressed is refused before sending ("submit a shorter range"). On success it prints the submission id, its status, the review link and, when other devices have minutes, the exact `--add` command to run on them. Uploads are never retried automatically, so a network failure can't create two submissions.

`--preview FILE` runs steps 1 to 4 and writes the exact, uncompressed request body to FILE (mode 0600: it holds the revealed rows), then stops. Nothing is uploaded and no confirmation is needed.

`--add <submission-id>` uploads this device's part of an existing submission to `POST /api/v1/submissions/{id}/parts`. Give the same project and range as the submission: the agent can't read a submission's metadata with an API key, and the server treats a part that lists minutes it didn't freeze as malformed. `--program` can't be combined with it. A device that can back none of the submission's minutes has nothing to add and stops with an error.

### Server answers

| Answer | Shown as |
|---|---|
| `401` | the server rejected the API key; run `logbook login` |
| `403` (JSON) | your Hackatime account is banned, so you can't submit hours |
| `400` (JSON) | the server refused the submission (or "can't take this range"), with the server's reason |
| `404` (JSON) | not found (an unknown program or submission), with the reason |
| `409` (JSON) | conflict: nothing to submit, a decision already recorded, or this device's part already added |
| `413` | too large; submit a shorter range |
| `429` | too many submissions; try again in <Retry-After> |
| network error, `5xx`, a redirect, or any answer without the server's JSON error body (a school filter's HTML `403`) | the error, "nothing was submitted; try again later" |

`serverapi.SubmitError` carries both: `errors.Is` matches the reason (`ErrBanned`, `ErrRefused`, `ErrNotFound`, `ErrConflict`, `ErrTooLarge`, `ErrKeyRejected`) and `errors.As` finds the `*serverapi.Error` with the status and message.

A part's answer may be `200`, `201`, `202` or `204`, with the same body as `POST /api/v1/submissions` or none; the agent then prints the submission id it was given and, when the answer has no `review_url`, points to the dashboard.

### Timeouts

The plan call has 3 minutes (`serverapi.PlanTimeout`) and reads at most 256 MiB; uploads have 20 minutes (`UploadTimeout`). The command's HTTP client is the daemon's (proxy from the environment, no redirects for POST) with those limits, and waits up to 5 minutes for the server's first byte after the body is sent, since the server checks a large body's shape before answering.

## `logbook verify`

```
logbook verify <bundle.json | bundle.json.gz>
```

Reads the verification bundle the admin API exports (gzip or plain JSON, told apart by the gzip header, not the file name; at most 1 GiB decompressed) and checks it with `pkg/` code only:

- per device: its signatures and chain (`submission.VerifyPart` with an empty part when the device uploaded none), then its part's proofs and replays (`VerifyPart`);
- the cap (`submission.CheckCap`);
- the whole bundle (`submission.CheckBundle`), whose answer decides.

It prints each step as OK or FAIL with the reason, then the packet's minute counts (verified, mismatch, unbacked), unbacked rows by reason, the first 20 mismatches (device's credit next to the replay's), and the counted hours from the frozen capped seconds. It exits 1 when any check fails. Mismatched and unbacked minutes are reported, not failures: the reviewer decides on them, as on the review page.
