// Package otlp is the agent's local OpenTelemetry receiver for Claude Code's
// active-time metric (claude_code.active_time.total, docs/STRUGGLE-RULES.md
// "Signals"). The daemon's loopback server (internal/agent/localapi) mounts
// it at POST /v1/metrics and POST /v1/logs.
//
// It accepts OTLP/HTTP with JSON encoding only (the Claude Code installer
// sets OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/json); protobuf requests get
// 415. Requests must carry X-Logbook-Token with the OTLP token derived from
// the local token (package localkey).
//
// From each request it keeps only the active-time datapoints, and of those
// only: session.id, type (user or cli), vcs.repository.name, the start and end
// times and the value. Every other attribute, including user.email,
// user.account_uuid, user.account_id, user.id, organization.id, terminal.type
// and host details, is never copied out of the decoder. Cumulative series are
// converted to deltas per (session.id, type) series with reset detection,
// using the last stored point of the series, so an agent restart doesn't
// double count. /v1/logs is accepted and discarded.
//
// Decoding and delta conversion are pure functions; storage is injected.
package otlp
