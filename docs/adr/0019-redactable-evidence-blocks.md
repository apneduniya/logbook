# 0019: Evidence blocks are salted minute buckets under a Merkle root

- Status: accepted
- Date: 2026-11-25
- Changes: the content hash and signed record of ADR 0017, point 7 and 8 (the canonical line encoding is unchanged), and adds a repo snapshot at the start of each block

## Context

A submission must let the server re-run the engine for each submitted minute on exactly the events the run at its key `(T, H)` read, and check those are events the device sealed on the day (ADR 0010, ADR 0017, ADR 0020). Today a block's `content_hash` is SHA-256 over the whole block's lines, so checking any row needs the whole day: every project, file path and AI session. Teens in the bounty thread were clear that nothing beyond what's needed should leave the laptop.

A design review (25 Nov) showed that revealing *some* rows under a plain per-row Merkle tree isn't enough:
1. **Completeness.** Membership proofs don't show that nothing was left out. A user could hide the activity samples of a minute (focus on another app) and the replay of what's left would credit it. Freezing the uploaded value doesn't help, because the uploaded value is what's being checked.
2. **Pruning.** Once unrelated rows of a block are deleted (90-day retention), the hashes needed to prove the rest can't be rebuilt. Repo mapping rows are written once and can sit in a months-old block.
3. **Guessing.** An unsalted hash of a single hidden row (say an activity sample) can be confirmed by trying candidates.

No device has sealed a day in production yet (one user, no devices on 24 Nov 2026), so the format can change now at no cost.

## Decision

**1. Buckets.** The rows of a block are grouped into buckets:
- one **repo bucket** holding the block's repo rows (key `0`);
- one **minute bucket** per UTC minute that has rows, keyed `1 + unix minute` of the row's time. The row time is the one the engine's window selection uses, defined once in `pkg/replay` (`RowTime`), so a run's window `[T − 120 min, T)` is exactly a range of minute buckets.

Each bucket gets a fresh random 32-byte **salt** when the block is sealed.
- `bucket_hash = SHA-256(0x02 ‖ salt ‖ lines)`, where `lines` are the bucket's canonical lines (ADR 0017 point 8) in `seq` order, each followed by `\n`.
- `leaf = SHA-256(0x00 ‖ key as 8-byte big-endian ‖ bucket_hash)`; `node = SHA-256(0x01 ‖ left ‖ right)`.
- Leaves are sorted by key. The tree shape is RFC 6962's (for `n > 1`, the left subtree holds the largest power of two smaller than `n`). `content_hash` is its root.

**2. The signed record grows.** Each evidence day also carries `buckets` (the leaf count), `time_from` and `time_to` (the first and last minute bucket's minute), and these go into the chain hash. The chain domain becomes `logbook-evidence-v2`. Agents older than this are refused at `evidence-days` (`400`, "update the agent").

**3. The agent keeps the bucket list.** For every sealed block it stores `(key, salt, bucket_hash)` per bucket (about 100 KB for a busy day) for as long as the chain exists. So hidden buckets can always be proven, even after their rows are pruned. A bucket that must be opened needs all its rows still present; if any were pruned, the minutes that need it are sent without evidence and show as **unbacked**.

**4. A repo snapshot opens every block.** In the same transaction that seals a block, the agent logs the current latest mapping per path as repo rows. They are the first rows of the next range. So the mapping a run at key `(T, H)` used (the latest per path with `seq ≤ H`) is exactly the repo rows with `seq ≤ H` in the repo bucket of the block whose range holds `H`. A device's first block holds every repo row ever written, because nothing is pruned before the first seal. The values the engine sees don't change: a snapshot row repeats the latest value for its path.

**5. Proofs.** For one block, a proof has:
- `buckets`: the leaf count `n`;
- `opened`: buckets revealed in full, each `{i, key, salt, lines}` (`i` is the 0-based leaf index);
- `edges`: neighbouring leaves revealed only as `{i, key, bucket_hash}`, to bound a range;
- `hashes`: the hash of every maximal subtree with no opened or edge leaf, in left-to-right order.

The verifier walks the tree from the root: a subtree with no listed leaf takes the next hash; an opened leaf is hashed from its salt and lines; an edge leaf from its key and hash. It's valid when every hash is used exactly once, the root equals the stored `content_hash`, keys increase with `i`, and every opened line decodes with `s` increasing within its bucket.

**6. Completeness of a window.** To show it revealed everything in minutes `[a, b)` of a block, a proof opens every bucket with a key in that range and gives as edges the leaves just before and just after it (or the range touches the first or last leaf). Keys of consecutive indices leave no room for a missing bucket. A block needs this only if its signed `[time_from, time_to]` overlaps `[a, b)` and its range starts at or below `H`. Every other block is known not to hold rows of the window.

**7. What a hidden bucket gives away:** that the minute had rows (from edge keys and the bucket count), nothing else. Salts make the hashes unguessable.

## What the re-check then proves

For every submitted minute: the server ran the stated rules on *all* the events the device logged, by the time of the run, for the 2 hours that the run read, and on the repo mapping it used. Those events were sealed and signed on the day, so the user couldn't choose afterwards what the check sees.

What it can't prove: that a modified agent logged the truth in the first place. That is the accepted risk in the threat model; flags and reviewers cover it.

## Consequences

- A submission uploads the opened buckets for its minutes' windows, the repo bucket of each key's block, a few edge leaves and subtree hashes. Size grows with the work submitted, not with the days.
- Sealing reads a range's rows grouped by `(minute, seq)` instead of `seq`, in chunks with a cursor, as before. The hasher state (the frontier plus the open bucket) stays small.
- The golden content hash in `pkg/evidence/testdata` and the e2e block checks change; the server's `evidence-days` handler stores the new fields. Agents must update to v0.2.0.
