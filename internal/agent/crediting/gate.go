package crediting

import (
	"context"
	"errors"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/upload"
)

// gate holds backoff and pause state, with the heartbeat uploader's rules:
// exponential backoff with jitter after failures (a longer Retry-After
// wins), and a pause after a 401 that lasts until the API key changes, with
// one retry with the same key every Reprobe.
type gate struct {
	failures  int
	nextTry   time.Time
	paused    bool
	pausedKey string
	probeAt   time.Time
	// reason is the persisted pause reason ("" when sending).
	reason string
	synced bool
	// revokedKey is the API key in use when the device was found revoked.
	revokedKey string
	// blocked, when non-empty, is why the server refused to register the
	// device (such as the active-device limit). It is shown while retrying
	// with backoff and cleared by a successful run or a new API key.
	blocked string
}

// reasonRevoked is the pause reason for a revoked device.
const reasonRevoked = "device revoked; run `logbook login` with a new API key to pair this machine again"

// ready reports whether to upload now, updating the persisted pause reason.
func (r *Runner) ready(ctx context.Context, t Target, now time.Time) bool {
	r.mu.Lock()
	g := &r.gate
	ok, reason := true, ""
	if g.revokedKey != "" && t.APIKey != g.revokedKey {
		g.revokedKey = ""
	}
	switch {
	case t.Disabled != "":
		ok, reason = false, t.Disabled
	case g.revokedKey != "":
		// Identity.Ensure re-checks the revocation locally, without a request.
		reason = reasonRevoked
	case g.paused && t.APIKey != g.pausedKey:
		// A new key (`logbook login`): start over.
		*g = gate{reason: g.reason, synced: g.synced}
	case g.paused && now.Before(g.probeAt):
		ok, reason = false, upload.ReasonReLogin
	case g.paused:
		reason = upload.ReasonReLogin
		g.probeAt = now.Add(r.opt.Reprobe)
	case now.Before(g.nextTry):
		ok = false
		reason = g.reason // keep whatever was shown
	default:
		reason = g.blocked
	}
	changed := !g.synced || g.reason != reason
	g.synced, g.reason = true, reason
	r.mu.Unlock()
	if changed {
		r.persist(ctx, localdb.KV{Key: keyPaused, Value: reason})
	}
	return ok
}

// setRevoked shows the device as revoked until the API key changes.
func (r *Runner) setRevoked(ctx context.Context, apiKey string) {
	r.mu.Lock()
	changed := r.gate.reason != reasonRevoked
	r.gate.reason, r.gate.revokedKey = reasonRevoked, apiKey
	r.mu.Unlock()
	if changed {
		r.persist(ctx, localdb.KV{Key: keyPaused, Value: reasonRevoked})
	}
}

// fail handles a failed upload step by its kind.
func (r *Runner) fail(ctx context.Context, t Target, err error) {
	now := r.opt.Now().UTC()
	switch serverapi.KindOf(err) {
	case serverapi.KindAuth:
		r.mu.Lock()
		r.gate.paused, r.gate.pausedKey, r.gate.probeAt = true, t.APIKey, now.Add(r.opt.Reprobe)
		r.gate.reason = upload.ReasonReLogin
		r.mu.Unlock()
		r.opt.Logger.ErrorContext(ctx, "API key rejected; crediting uploads paused until the key changes")
		r.persist(ctx, localdb.KV{Key: keyPaused, Value: upload.ReasonReLogin},
			localdb.KV{Key: keyLastError, Value: err.Error()}, localdb.KV{Key: keyLastErrorAt, Value: formatTime(now)})
		return
	case serverapi.KindRevoked:
		repaired, merr := r.opt.Identity.MarkRevoked(ctx, t.Target)
		if merr != nil {
			r.opt.Logger.ErrorContext(ctx, "recording device revocation", "err", merr)
		}
		if !repaired {
			r.opt.Logger.ErrorContext(ctx, "the server revoked this device; uploads stopped", "err", err)
			r.setRevoked(ctx, t.APIKey)
			r.persist(ctx, localdb.KV{Key: keyLastError, Value: err.Error()}, localdb.KV{Key: keyLastErrorAt, Value: formatTime(now)})
			return
		}
		// The device was revoked before this API key was ever used with it
		// (the user already ran `logbook login`): a new device registers
		// after the usual backoff.
	case serverapi.KindConflict:
		// Only registration answers 409 here (evidence conflicts are handled
		// per day), and not for a key of another account, which Identity
		// handles: the server refuses another device, e.g. the active-device
		// limit. Show it and retry with backoff; the key is kept.
		blocked := "the server refused to register this device: " + serverMessage(err)
		r.mu.Lock()
		r.gate.blocked, r.gate.reason = blocked, blocked
		r.mu.Unlock()
		r.persist(ctx, localdb.KV{Key: keyPaused, Value: blocked})
	case serverapi.KindUnknownDevice:
		// Register again on the next attempt (after backoff, so a server
		// that keeps forgetting the device isn't hammered).
		if ferr := r.opt.Identity.Forget(ctx); ferr != nil {
			r.opt.Logger.ErrorContext(ctx, "forgetting device id", "err", ferr)
		}
	}
	r.mu.Lock()
	r.gate.failures++
	wait := upload.Backoff(r.gate.failures, r.opt.MinBackoff, r.opt.MaxBackoff, r.opt.Jitter())
	if ra := serverapi.RetryAfterOf(err); ra > wait {
		wait = min(ra, maxRetryAft)
	}
	r.gate.nextTry = now.Add(wait)
	failures := r.gate.failures
	r.mu.Unlock()
	r.opt.Logger.WarnContext(ctx, "crediting upload failed; backing off", "err", err, "attempt", failures,
		"wait", wait.Round(time.Second))
	r.persist(ctx, localdb.KV{Key: keyLastError, Value: err.Error()}, localdb.KV{Key: keyLastErrorAt, Value: formatTime(now)})
}

// serverMessage is the server's own error text when there is one.
func serverMessage(err error) string {
	var e *serverapi.Error
	if errors.As(err, &e) && e.Message != "" {
		return e.Message
	}
	return err.Error()
}

// succeed clears backoff and pause after a fully successful run, and tells
// the identity that t's API key works for this device.
func (r *Runner) succeed(ctx context.Context, t serverapi.Target, now time.Time) {
	if err := r.opt.Identity.Confirm(ctx, t); err != nil {
		r.opt.Logger.WarnContext(ctx, "recording the working API key", "err", err)
	}
	r.mu.Lock()
	wasPaused := r.gate.paused
	r.gate.failures, r.gate.nextTry = 0, time.Time{}
	r.gate.paused, r.gate.pausedKey, r.gate.probeAt = false, "", time.Time{}
	r.gate.reason, r.gate.blocked = "", ""
	r.batch = min(r.batch*2, r.opt.Batch)
	r.mu.Unlock()
	if wasPaused {
		r.opt.Logger.InfoContext(ctx, "API key accepted again; crediting uploads resumed")
	}
	r.persist(ctx, localdb.KV{Key: keyLastSuccess, Value: formatTime(now)}, localdb.KV{Key: keyPaused, Value: ""},
		localdb.KV{Key: keyLastError, Value: ""}, localdb.KV{Key: keyLastErrorAt, Value: ""})
}
