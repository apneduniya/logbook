// Package submissions stores submissions of hours and serves their review
// (ADR 0020, docs/api/submissions.md):
//
//   - GET /api/v1/me/submissions/plan (API key): the rows the agent builds a
//     bundle for;
//   - POST /api/v1/submissions and POST /api/v1/submissions/{id}/parts (API
//     key, gzip): shape checks, the Hackatime trust gate, and, at creation,
//     freezing the rows, capped seconds and cap inputs from one snapshot;
//   - the re-check job (River queue "submissions", Checker.Verify): each part
//     verified with pkg/submission against the device's stored evidence
//     chain (package ledger) and public key, then the review packet;
//   - the review link: GET /api/v1/review/{token} (no auth) and POST
//     /api/v1/review/{token}/decision (a program member, never the
//     submitter);
//   - the submitter's list, detail and withdrawal (/api/v1/me/submissions*);
//   - the program's inbox: GET /api/v1/me/apps/{id}/submissions (members)
//     and GET /api/v1/submissions[/{id}] (the app's client-credentials token
//     with scope submissions);
//   - the retention job that deletes stored parts one year after the latest
//     decision (or the submission, with none);
//   - the audit entries submission.create, submission.withdraw and
//     submission.decide.
//
// What a submission means (wire types, verifier, cap, packet) belongs to
// pkg/submission; this package only loads, stores and serves. It doesn't
// own evidence records (ledger), devices, program membership (oauth, whose
// GetApp it asks) or Hackatime trust (hackatime, whose refresher it calls).
//
// # Review tokens
//
// A review link carries a 128-bit random token. Only its SHA-256 is used to
// look it up, and an AES-256-GCM copy (secretbox, the submission id as
// associated data) lets the submitter and the program's members see the
// link again. The token is never logged, audited or stored in the packet.
package submissions
