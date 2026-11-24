// Package evidence defines Logbook's tamper-evident evidence blocks (ADR
// 0010, ADR 0017, ADR 0019), so agent and server use exactly one copy of
// every format:
//
//   - the canonical line encoding of a logged row (Row, Row.Line, DecodeLine);
//   - how a block's rows are grouped into salted buckets and hashed into a
//     Merkle root, its content hash (Hasher, HashBlock);
//   - proofs that reveal some buckets of a sealed block and hide the rest,
//     and the check that a proof reveals every row of a range of minutes
//     (Proof, BuildProof, VerifyProof, PlanProof, Verified.CheckComplete);
//   - the signed record that chains one block per UTC day, and its ed25519
//     signature (Day, ChainHash, Sign, Verify).
//
// It does no I/O. It doesn't read the log, decide which minutes a
// submission covers, or run the engine (pkg/replay does that).
package evidence
