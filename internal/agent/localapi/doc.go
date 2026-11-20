// Package localapi is the agent's loopback HTTP server (ADR 0009). It serves:
//
//   - the WakaTime-compatible heartbeat endpoints wakatime-cli posts to
//     (POST /api/v1/users/current/heartbeats and .bulk, also under
//     /api/hackatime/v1), which queue heartbeats locally and answer
//     immediately, never waiting on the network;
//   - GET .../users/current/statusbar/today, proxied to the Logbook server
//     with a short timeout and a 30-second cache, falling back to a local
//     estimate so editors never see an error;
//   - POST /local/v1/events, where hook commands send wire.LocalEvent
//     batches, authenticated with the local token;
//   - GET / (the status page) and GET /local/v1/status (JSON for the CLI);
//   - POST /v1/metrics and /v1/logs, Claude Code's OpenTelemetry export,
//     when an OTLP handler is injected (package otlp authenticates and
//     parses it).
//
// Every request must come from a loopback address with a Host header of
// 127.0.0.1:<port> or localhost:<port> (DNS-rebinding defence), and is
// refused if a browser marks it cross-site. It owns request parsing and
// validation only; storage is injected (Store) and so is the status source.
package localapi
