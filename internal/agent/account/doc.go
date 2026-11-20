// Package account checks a Logbook API key against the server, for
// `logbook login` and `logbook doctor`. It calls the WakaTime status-bar
// endpoint, the lightest authenticated read the server has, and reports the
// server's clock from the Date header so callers can detect clock skew. It
// never logs or returns the key.
package account
