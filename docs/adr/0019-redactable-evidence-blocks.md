# 0019: Evidence blocks are salted minute buckets under a Merkle root

- Status: accepted
- Date: 2026-11-25
- Changes: ADR 0017 points 7 and 8: the content hash, the signed record, and how a block's upper bound is chosen (the canonical line encoding is unchanged); adds a repo snapshot at the start of each block

## Context

A submission must let the server re-run the engine for each submitted minute on exactly the events the run at its key `(T, H)` read, and check those are events the device sealed on the day (ADR 0010, ADR 0017, ADR 0020). Today a block's `content_hash` is SHA-256 over the whole block's lines, so checking any row needs the whole day: every project, file path and AI session. Teens in the bounty thread were clear that nothing beyond what's needed should leave the laptop.

A design review (25 Nov) showed that revealing *some* rows under a plain per-row Merkle tree isn't enough:
1. **Completeness.** Membership proofs don't show that nothing was left out. A user could hide the activity samples of a minute (focus on another app) and the replay of what's left would credit it. Freezing the uploaded value doesn't help, because the uploaded value is what's being checked.
2. **Pruning.** Once unrelated rows of a block are deleted (90-day retention), the hashes needed to prove the rest can't be rebuilt. Repo mapping rows are written once and can sit in a months-old block.
3. **Guessing.** An unsalted hash of a single hidden row (say an activity sample) can be confirmed by trying candidates.

No device has sealed a day in production yet (one user, no devices on 24 Nov 2026), so the format can change now at no cost.

## Decision

**1. Buckets.** The rows of a block are grouped into buckets:
- one **repo bucket** holding the block's repo rows (key `0`). It is always present, empty when the block has no repo rows. It holds repo rows only, whatever their timestamps;
- one **minute bucket** per UTC minute that has other rows, keyed `1 + unix minute` of the row's time (`evidence.Row.Time`). For heartbeats, AI, activity and git rows that is the time the engine's window selection uses, so a run's window `[T − 120 min, T)` is exactly a range of minute buckets. Active-time rows are timed at their `To` but selected by overlap; see point 8.

Each bucket gets a fresh random 32-byte **salt** when the block is sealed.
- `bucket_hash = SHA-256(0x02 ‖ salt ‖ lines)`, where `lines` are the bucket's canonical lines (ADR 0017 point 8) in `seq` order, each followed by `\n`.
- `leaf = SHA-256(0x00 ‖ key as 8-byte big-endian ‖ bucket_hash)`; `node = SHA-256(0x01 ‖ left ‖ right)`.
- Leaves are sorted by key. The tree shape is RFC 6962's (for `n > 1`, the left subtree holds the largest power of two smaller than `n`). `content_hash` is its root.

**2. The signed record grows.** Each evidence day also carries:
- `seq_from` and `seq_to`: the block holds exactly the rows with `seq_from < seq ≤ seq_to`. A device's first block has `seq_from = 0`; every later block's `seq_from` equals the previous block's `seq_to`, which the server checks along with `prev_chain_hash`. `seq_to > seq_from`;
- `buckets`: the leaf count;
- `time_from` and `time_to`: the first and last minute bucket's minute, or both `null` for a block with only a repo bucket.

The chain hash becomes `SHA-256("logbook-evidence-v2" ‖ device_id ‖ day ‖ prev_chain_hash ‖ content_hash ‖ rules_version ‖ seq_from ‖ seq_to ‖ buckets ‖ time_from ‖ time_to)`, with the exact byte encoding (lengths, integer widths, the `null` form) pinned in `pkg/evidence` by a golden test. Agents older than this are refused at `evidence-days` (`400`, "update the agent").

**3. The agent keeps the bucket list.** For every sealed block it stores `(key, salt, bucket_hash)` per bucket (about 100 KB for a busy day) for as long as the chain exists. So hidden buckets can always be proven, even after their rows are pruned. A bucket that must be opened needs all its rows still present; if any were pruned, the minutes that need it are sent without evidence and show as **unbacked**.

**4. The upper bound is fixed first, and a repo snapshot opens every block.** ADR 0017 let a block's upper bound move with each chunk's snapshot. With rows grouped by minute that would miss a late row added behind the cursor, so instead:
- In one transaction, once the sealing conditions of ADR 0017 hold, the agent reads the high-water mark, records it as the block's `seq_to` (persisted with the sealing progress), and logs the current latest mapping per path as repo rows. Those snapshot rows take the next sequence numbers, so they are the first rows of the next range. This write bypasses `RecordRepos`' skip-if-unchanged rule.
- Hashing then reads only rows with `seq_from < seq ≤ seq_to`, grouped by `(bucket key, seq)`, in chunks with a cursor. Nothing can be added to that range afterwards, so the cursor can't miss a row.
- The same transaction **reserves the day**: it records (durably, so it survives a restart) that day `D` is being sealed, and from then on no run computes or stores any minute of `D` or earlier, exactly as if `sealed_through` had already moved past it. Without this, a clock set back during a long hash could compute a minute of `D` that was never computed before, at an `H` above the frozen bound. So every minute of the day has a key `H ≤ seq_to`: the ones that exist were final and acknowledged when the bound was frozen, and no new one can appear.
- Pruning never deletes rows of a range until its root, record and bucket list are stored.

The mapping a run at key `(T, H)` used is the latest row per path with `seq ≤ H`. In the block whose range holds `H`, that is: the repo bucket's rows with `seq ≤ H`, reduced to the highest `seq` per exact stored path. `pkg/replay` does that reduction before building `Input.Repos`, because the engine itself resolves equal paths by project name, not by sequence.

**A device's first block.** On a first install it starts at `seq_from = 0` and holds every repo row ever written (nothing is pruned before the first seal). When a new device replaces the previous one (ADR 0017 point 5), older rows may already be pruned, so the takeover itself sets the baseline: in one transaction the agent records the log's high-water mark as the new device's first `seq_from` and logs a repo snapshot (the latest mapping per path), which become its first rows. Minutes the new device computes read windows at or after its registration, which the existing deletion-floor guard already keeps intact.

**5. Proofs.** For one block, a proof has:
- `buckets`: the leaf count `n`;
- `opened`: buckets revealed in full, each `{i, key, salt, lines}` (`i` is the 0-based leaf index);
- `edges`: neighbouring leaves revealed only as `{i, key, bucket_hash}`, to bound a range;
- `hashes`: the hash of every maximal subtree with no opened or edge leaf, in left-to-right order.

The verifier walks the tree from the root: a subtree with no listed leaf takes the next hash; an opened leaf is hashed from its salt and lines; an edge leaf from its key and hash. It's valid when every hash is used exactly once, the root equals the stored `content_hash`, keys increase with `i`, and every opened line decodes with `s` increasing within its bucket.

**6. Completeness of a window.** To show it revealed everything in minutes `[a, b)` of a block, a proof opens every minute bucket with a key in that range and gives as edges the leaves just before and just after it (or the range touches the last leaf; the repo bucket, key `0`, always sits before every minute bucket). Keys of consecutive indices leave no room for a missing bucket. A block needs this only if its signed `[time_from, time_to]` overlaps `[a, b)` and `seq_from < H`. Every other block is known, from signed fields, not to hold rows of the window with `seq ≤ H`. The verifier then keeps only opened rows with `seq ≤ H`.

Verification also checks that every opened row's `seq` lies in `(seq_from, seq_to]`, that repo rows appear only in the repo bucket and every other type only in the minute bucket of its time, and that `seq` increases within each bucket.

**7. What a hidden bucket gives away:** that the minute had rows (from edge keys and the bucket count), nothing else. Salts make the hashes unguessable.

**8. Active time is outside the guarantee.** Claude Code's active-time points are intervals, selected by overlap and read outside the run's snapshot, and they feed only the advisory `otel_mismatch` flag. Replays compare label, project, credit and cuts, never flags (ADR 0017), so active-time rows are neither needed nor proven complete. Opened buckets may contain some; they are ignored.

## What the re-check then proves

For every submitted minute: the server ran the stated rules on *all* the events (other than active time, point 8) the device logged, by the time of the run, for the 2 hours that the run read, and on the repo mapping it used. Those events were sealed and signed on the day, so the user couldn't choose afterwards what the check sees.

What it can't prove: that a modified agent logged the truth in the first place. That is the accepted risk in the threat model; flags and reviewers cover it.

## Consequences

- A submission uploads the opened buckets for its minutes' windows, the repo bucket of each key's block, a few edge leaves and subtree hashes. Size grows with the work submitted, not with the days.
- Sealing fixes `seq_to` first, then reads the range grouped by `(bucket key, seq)` in chunks with a cursor. The hasher state (the frontier plus the open bucket) stays small.
- The server's `evidence_days` gains `seq_from`, `seq_to`, `buckets`, `time_from`, `time_to` (new migration).
- The golden content hash in `pkg/evidence/testdata` and the e2e block checks change; the server's `evidence-days` handler stores the new fields. Agents must update to v0.2.0.
