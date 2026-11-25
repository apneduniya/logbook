// Package audit is `logbook verify` (ADR 0020 "Checking it without us"):
// it reads a submission's verification bundle (the JSON the admin API
// exports, gzip-compressed or not) and checks it step by step with
// pkg/submission: each device's signatures and chain, each part's proofs
// and replays, and the multi-device cap. It reports the same minute counts
// and counted seconds as the review packet, and whether every check
// passed.
//
// It imports only pkg/ and the standard library, never another internal
// package, so an auditor checks a submission with the same code the
// server runs and nothing else (a test enforces this). It owns the
// report's wording; it doesn't own any rule of the check.
package audit
