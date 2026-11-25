// Package submit is `logbook submit` (ADR 0020 "The bundle"): it asks the
// server which minute rows it holds for a project and range, builds this
// device's evidence part for them (proofs from crediting.BuildProofs),
// tells the user exactly what the part reveals, and uploads it only after
// they confirm (or passed --yes), either as a new submission or as a part
// added to an existing one (--add). --preview writes the body to a file
// and uploads nothing.
//
// It owns which of this device's minutes a part lists and why the others
// can't be backed, the summary shown before uploading, the confirmation,
// and refusing ranges that can't be submitted yet. It doesn't own the
// proofs (internal/agent/crediting), the wire types and their checks
// (pkg/submission), or the HTTP calls (internal/agent/serverapi).
package submit
