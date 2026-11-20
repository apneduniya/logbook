// Package struggle is the crediting engine: it turns a device's raw events
// into labelled, credited minutes (docs/STRUGGLE-RULES.md, ADR 0010, ADR
// 0012). It is pure and deterministic: no I/O, no clock, no randomness, no
// dependence on input order. The agent runs it continuously; the server
// re-runs it on submitted evidence.
//
// It owns labelling (you, agent, idle), project attribution, the cut rules
// and the reviewer flags of one rules version. It does not own storage,
// uploads, evidence hashing (pkg/evidence) or checks that need more than one
// device (multi_device is set by the server).
package struggle
