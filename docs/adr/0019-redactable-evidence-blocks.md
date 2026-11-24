# 0019: Evidence blocks are Merkle trees, so a submission reveals only what it needs

- Status: accepted
- Date: 2026-11-25
- Changes: the content hash of ADR 0017, point 8 (the canonical line encoding is unchanged)

## Context

A submission must let the server re-run the engine on the raw events behind each submitted minute and check that those events are the ones the device sealed on the day (ADR 0010, ADR 0017). Today a block's `content_hash` is SHA-256 over the whole block's canonical lines. To check any one row, the server would need the whole block, which is every event of that UTC day: every project, every file path, every AI session.

Teens in the bounty thread were clear that nothing beyond what's needed should leave the laptop. A submission for one project shouldn't upload the user's other projects for the whole day.

No device has sealed a day in production yet (24 Nov 2026: one user, no devices), so the hash can change now at no cost.

## Decision

**1. A block's `content_hash` is the root of a Merkle tree over its canonical lines.**
- Leaves are the block's lines in `seq` order, each line exactly as ADR 0017 point 8 encodes it, without the trailing `\n`.
- `leaf = SHA-256(0x00 ‖ line)`, `node = SHA-256(0x01 ‖ left ‖ right)`.
- The tree shape is RFC 6962's: for `n > 1` leaves, the left subtree holds the largest power of two smaller than `n`. A block with no rows is never sealed (a gap), so `n ≥ 1`.
- `pkg/evidence` builds it incrementally: the hasher keeps the frontier of perfect subtrees (one hash per set bit of the leaf count), so a large first block is still hashed in chunks over several runs, and the saved hasher state stays a few kilobytes.
- `event_counts` stays as it is. The leaf count `n` is sent with each block (`rows`) and stored.

**2. The chain domain becomes `logbook-evidence-v2`.** A v1 chain hash can never be read as a v2 one. Agents older than this change are refused at `evidence-days` (`400`, "update the agent"). There are no v1 days in production.

**3. A proof reveals some leaves and hides the rest behind subtree hashes.** For one block, a proof is:
- `rows`: the leaf count `n`;
- `revealed`: the revealed leaves, each `{i, line}`, with `i` the 0-based leaf index, strictly increasing;
- `hashes`: the hashes of every maximal subtree that has no revealed leaf, in left-to-right order.

The verifier walks the tree from the root. A subtree with no revealed leaf takes the next hash from `hashes`; a single revealed leaf is hashed from its line; anything else recurses. The proof is valid when every hash is used exactly once and the result equals the stored `content_hash`. It also checks that each revealed line decodes, and that `s` increases with `i`. Hidden runs of rows collapse into a handful of hashes, so a proof's size grows with what it reveals, not with the block.

**4. What a hidden row gives away:** only that a row exists at that position. Its type, time and content stay on the device.

## Why hiding rows is safe

The server can't check that a proof reveals every row a minute's replay needs: a hidden row's time is unknown. That doesn't weaken anything:
- The minutes being checked were uploaded and frozen before the submission (ADR 0017). Hiding a row that the agent used can only make the server's replay disagree, which is flagged. It can't raise credit.
- A modified agent that wants more credit can already leave events out of its log in the first place. That is the accepted risk in the threat model ("a determined user with a modified agent can fabricate activity in real time"). Hiding adds no new power.

What the chain proves is therefore: the revealed rows are exactly what the device logged and sealed on that day, and the frozen minutes follow from them under the stated rules.

## Consequences

- A submission uploads only the rows inside the replay windows of its minutes (ADR 0020), plus a few hashes per block.
- The golden content hash in `pkg/evidence/testdata` and the e2e block checks change. The agent's crediting and the server's `evidence-days` handler are otherwise unchanged: the server still stores and chains one hash per day.
- Agents built before v0.2.0 must update. The installer's release becomes v0.2.0.
