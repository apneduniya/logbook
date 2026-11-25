// Package serverapi is the agent's client for the Logbook server's crediting
// endpoints (ADR 0010): device registration (POST /api/v1/devices), credited
// minute uploads (POST /api/v1/devices/{id}/minutes) and evidence day uploads
// (POST /api/v1/devices/{id}/evidence-days); and for its submission
// endpoints (ADR 0020): the plan (GET /api/v1/me/submissions/plan) and
// gzip-compressed uploads (POST /api/v1/submissions and
// /api/v1/submissions/{id}/parts).
//
// It owns the request and response shapes, authentication (the user's API
// key as a Bearer token), the User-Agent, response size limits, the
// submission calls' timeouts and the classification of every answer into
// an error Kind (and, for submissions, a reason) callers act on. It does
// not decide when to call, retry or back off (internal/agent/crediting,
// internal/agent/submit), and it never logs or returns the API key.
package serverapi
