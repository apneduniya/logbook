// Package admin is the read-only admin API for Fraud Squad (ADR 0022,
// docs/api/admin.md) and the admin role and keys behind it:
//
//   - Keys: the server command line's `admin grant`, `admin revoke`,
//     `admin key create`, `admin key list` and `admin key revoke` (Command
//     parses them), and authentication of an admin key on a request;
//   - Handler: every route under /api/admin/v1/, behind a middleware that
//     takes only an admin key (Authorization: Bearer lba_…; sessions, user
//     API keys and OAuth tokens get 401), limits each key, and writes an
//     admin.read audit row for every request before its answer (Keys
//     writes them, parameters capped and recorded once per batch); it also
//     bounds how many bundle exports run at once;
//   - Service: the reads those routes serve. The Hackatime-compatible calls
//     (user/info, info_batch, heartbeats, projects, trust_logs,
//     get_user_by_email, search_fuzzy) match Hackatime at 43bde10
//     (app/controllers/concerns/api/admin/v1/user_utilities.rb); the
//     Logbook calls show devices and their evidence chains, credited
//     minutes, submissions with their decisions, parts and frozen rows, a
//     part's revealed rows and the verification bundle.
//
// Admin keys are "lba_" plus 256 random bits (base64url); only their
// SHA-256 is stored, looked up by the 12 characters after "lba_" and
// compared in constant time. A key works only while it isn't revoked and
// its user's role is admin.
//
// The API never changes user data: Service reads through Reads, whose
// queries (store/queries/admin.sql) are all SELECTs, and the request path's
// only writes are its audit rows and a key's last use. It doesn't own
// submissions (package submissions builds bundles and decodes parts, within
// its decode slots), evidence chains (ledger) or trust levels (hackatime).
package admin
