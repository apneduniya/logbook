// Package serverapi is the agent's client for the Logbook server's crediting
// endpoints (ADR 0010): device registration (POST /api/v1/devices), credited
// minute uploads (POST /api/v1/devices/{id}/minutes) and evidence day uploads
// (POST /api/v1/devices/{id}/evidence-days).
//
// It owns the request and response shapes, authentication (the user's API
// key as a Bearer token), the User-Agent, response size limits and the
// classification of every answer into an error Kind callers act on. It does
// not decide when to call, retry or back off (internal/agent/crediting), and
// it never logs or returns the API key.
package serverapi
