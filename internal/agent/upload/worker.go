package upload

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// Defaults for a Worker.
const (
	DefaultInterval   = 10 * time.Second
	DefaultBatch      = 100
	DefaultMinBackoff = 5 * time.Second
	DefaultMaxBackoff = 10 * time.Minute
	// maxRetryAfter caps how long a server's Retry-After can park a worker.
	maxRetryAfter = 30 * time.Minute
	// DefaultReprobe is how often a worker paused by a 401/403 tries once
	// more with the same key. A 403 can come from a school web filter rather
	// than the server, so a pause must not last for the daemon's lifetime.
	DefaultReprobe = 30 * time.Minute
	// flushTimeout bounds the final upload attempt on shutdown.
	flushTimeout = 5 * time.Second
	// maxBatchesPerCycle lets a large backlog drain in one cycle while still
	// returning to the loop (and noticing shutdown) regularly.
	maxBatchesPerCycle = 50
)

// Target is where a worker sends its queue, resolved on every attempt.
type Target struct {
	// URL is the full bulk endpoint, e.g. https://host/api/v1/users/current/heartbeats.bulk.
	URL    string
	APIKey string
	// Headers are extra request headers.
	Headers map[string]string
	// Disabled, when non-empty, says why nothing should be sent (e.g. "not signed in").
	Disabled string
}

// LogbookTarget returns the Logbook server target for cfg.
func LogbookTarget(cfg config.Config) Target {
	t := Target{
		URL:     strings.TrimRight(cfg.ServerURL, "/") + "/api/v1/users/current/heartbeats.bulk",
		APIKey:  cfg.APIKey,
		Headers: map[string]string{"X-Logbook-Source": "agent"},
	}
	switch {
	case cfg.APIKey == "":
		t.Disabled = "not signed in; run `logbook login`"
	case !validBase(cfg.ServerURL):
		t.Disabled = "the server URL in config.json is invalid; run `logbook login --server <url>`"
	}
	return t
}

// MirrorEnabled reports whether heartbeats should be queued for Hackatime.
func MirrorEnabled(cfg config.Config) bool {
	return cfg.Hackatime.Mirror && cfg.Hackatime.URL != "" && cfg.Hackatime.APIKey != ""
}

// HackatimeTarget returns the Hackatime mirror target for cfg. Its URL is the
// api_url saved from the user's original .wakatime.cfg, such as
// https://hackatime.hackclub.com/api/hackatime/v1.
func HackatimeTarget(cfg config.Config) Target {
	t := Target{
		URL:    strings.TrimRight(cfg.Hackatime.URL, "/") + "/users/current/heartbeats.bulk",
		APIKey: cfg.Hackatime.APIKey,
	}
	switch {
	case !MirrorEnabled(cfg):
		t.Disabled = "mirroring to Hackatime is off"
	case !validBase(cfg.Hackatime.URL):
		t.Disabled = "the saved Hackatime URL is invalid; run `logbook install` again"
	}
	return t
}

// validBase reports whether raw is an absolute http(s) URL with a host, so
// that a bad saved URL pauses the worker instead of failing every request.
func validBase(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

// Store is the queue and state storage a Worker needs. *localdb.DB satisfies it.
type Store interface {
	Peek(ctx context.Context, q localdb.Queue, n int) ([]localdb.Item, error)
	Delete(ctx context.Context, q localdb.Queue, ids []int64) error
	Pending(ctx context.Context, q localdb.Queue) (int64, error)
	SetMany(ctx context.Context, kvs ...localdb.KV) error
	Add(ctx context.Context, key string, n int64) error
}

// Options configures a Worker. Zero durations take the defaults.
type Options struct {
	// Name is a short label ("logbook" or "hackatime") used in logs and state keys.
	Name   string
	Queue  localdb.Queue
	Store  Store
	Target func() Target
	// Client is shared by all workers; it must have a timeout.
	Client    *http.Client
	UserAgent string
	Logger    *slog.Logger

	Interval, MinBackoff, MaxBackoff time.Duration
	// Reprobe is how long a 401/403 pause lasts before one retry with the
	// same key (DefaultReprobe).
	Reprobe time.Duration
	Batch   int
	Now     func() time.Time
	// Jitter returns a value in [0, 1); math/rand/v2 by default.
	Jitter func() float64
}

// Worker drains one queue. Create it with New and start it with Run.
type Worker struct {
	opt  Options
	kick chan struct{}

	mu       sync.Mutex
	failures int
	nextTry  time.Time
	// pausedKey is the API key that was rejected; the worker stays paused
	// until the configured key differs from it or a re-probe succeeds.
	pausedKey string
	paused    bool
	// probeAt is when a paused worker tries the same key once more.
	probeAt time.Time
	// batch is the current batch size: Options.Batch, halved while the
	// server refuses whole batches as too large or malformed.
	batch int
	// synced is set once the pause reason has been written to the store, so
	// a stale reason left by an earlier run is always overwritten.
	synced bool
	state  State
}

// New returns a Worker.
func New(o Options) *Worker {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	if o.Batch <= 0 || o.Batch > DefaultBatch {
		o.Batch = DefaultBatch
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64 //nolint:gosec // jitter needs no cryptographic randomness
	}
	if o.Reprobe <= 0 {
		o.Reprobe = DefaultReprobe
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Worker{opt: o, kick: make(chan struct{}, 1), batch: o.Batch, state: State{Name: o.Name}}
}

// Name returns the worker's label.
func (w *Worker) Name() string { return w.opt.Name }

// Kick asks the worker to check its queue now. It never blocks; the worker
// sends early only when a full batch is waiting and it is not backing off.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// State returns the worker's live state.
func (w *Worker) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// Run sends the queue until ctx is done, then makes one last bounded
// attempt to flush what is queued. It always returns nil.
func (w *Worker) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			w.flush(ctx)
			return nil
		case <-timer.C:
		case <-w.kick:
			if !w.fullBatchWaiting(ctx) {
				continue
			}
		}
		timer.Reset(w.cycle(ctx))
	}
}

// flush makes one final attempt after shutdown began. The queue is durable,
// so whatever doesn't make it is sent on the next start.
func (w *Worker) flush(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), flushTimeout)
	defer cancel()
	w.mu.Lock()
	backingOff := w.opt.Now().Before(w.nextTry)
	w.mu.Unlock()
	if backingOff {
		return
	}
	w.cycle(ctx)
}

func (w *Worker) fullBatchWaiting(ctx context.Context) bool {
	w.mu.Lock()
	backingOff := w.opt.Now().Before(w.nextTry)
	w.mu.Unlock()
	if backingOff {
		return false
	}
	n, err := w.opt.Store.Pending(ctx, w.opt.Queue)
	return err == nil && n >= int64(w.opt.Batch)
}

// cycle sends batches until the queue is empty, a failure occurs, or the
// per-cycle limit is hit, and returns how long to wait before the next one.
func (w *Worker) cycle(ctx context.Context) time.Duration {
	t := w.opt.Target()
	if !w.ready(ctx, t) {
		return w.opt.Interval
	}
	for range maxBatchesPerCycle {
		if ctx.Err() != nil {
			return w.opt.Interval
		}
		size := w.batchSize()
		items, err := w.opt.Store.Peek(ctx, w.opt.Queue, size)
		if err != nil {
			w.opt.Logger.ErrorContext(ctx, "reading upload queue", "worker", w.opt.Name, "err", err)
			return w.opt.Interval
		}
		if len(items) == 0 {
			return w.opt.Interval
		}
		r := send(ctx, w.opt.Client, w.opt.UserAgent, t, items)
		if r.outcome == outcomeBatchRejected {
			if len(items) > 1 {
				// Find the item at fault: retry at once with smaller batches.
				w.shrink(len(items))
				w.opt.Logger.WarnContext(ctx, "server refused a batch; splitting it",
					"worker", w.opt.Name, "size", len(items), "status", r.status, "err", r.err)
				continue
			}
			// One heartbeat the server refuses on its own will never succeed.
			r.rejected = []int64{items[0].ID}
		}
		if err := w.apply(ctx, r); err != nil {
			w.opt.Logger.ErrorContext(ctx, "updating upload queue", "worker", w.opt.Name, "err", err)
			return w.opt.Interval
		}
		switch r.outcome {
		case outcomeAuth:
			w.pause(ctx, t.APIKey, r.err)
			return w.opt.Interval
		case outcomeBatchRejected, outcomeRetry:
			// Back off after a dropped single item too, so a server that
			// refuses everything can't empty the queue quickly.
			return w.fail(ctx, r)
		case outcomeOK:
			// A per-item answer means the key was accepted.
			w.resume(ctx)
		}
		if r.retry > 0 {
			return w.fail(ctx, r)
		}
		w.succeed(ctx, len(r.done))
		if len(items) < size {
			return w.opt.Interval
		}
	}
	return 0 // more is waiting; come straight back
}

func (w *Worker) batchSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.batch
}

// shrink halves the batch size below n (at least 1).
func (w *Worker) shrink(n int) {
	w.mu.Lock()
	w.batch = max(1, min(w.batch, n)/2)
	w.mu.Unlock()
}

// ready reports whether the worker may send to t, updating its pause state.
func (w *Worker) ready(ctx context.Context, t Target) bool {
	now := w.opt.Now()
	w.mu.Lock()
	ok := true
	reason := ""
	switch {
	case t.Disabled != "":
		ok, reason = false, t.Disabled
	case w.paused && t.APIKey != w.pausedKey:
		// The key changed (e.g. after `logbook login`): try again.
		w.paused, w.pausedKey, w.probeAt = false, "", time.Time{}
		w.failures, w.nextTry = 0, time.Time{}
	case w.paused && now.Before(w.probeAt):
		ok, reason = false, ReasonReLogin
	case w.paused:
		// Try the same key once more: the 401/403 may have come from a
		// filtering proxy, or the key may have been restored. Stay paused
		// until a request is actually accepted.
		reason = ReasonReLogin
		w.probeAt = now.Add(w.opt.Reprobe)
	}
	changed := !w.synced || w.state.Paused != reason
	w.synced = true
	w.state.Paused = reason
	w.mu.Unlock()
	if changed {
		w.persist(ctx, localdb.KV{Key: stateKey(w.opt.Name, keyPaused), Value: reason})
	}
	return ok
}

// resume clears a re-login pause once the server accepted a request.
func (w *Worker) resume(ctx context.Context) {
	w.mu.Lock()
	wasPaused := w.paused
	w.paused, w.pausedKey, w.probeAt = false, "", time.Time{}
	if w.state.Paused == ReasonReLogin {
		w.state.Paused = ""
	}
	w.mu.Unlock()
	if wasPaused {
		w.opt.Logger.InfoContext(ctx, "upload key accepted again; resuming", "worker", w.opt.Name)
		w.persist(ctx, localdb.KV{Key: stateKey(w.opt.Name, keyPaused), Value: ""})
	}
}

// apply deletes accepted and rejected items and counts rejections.
func (w *Worker) apply(ctx context.Context, r result) error {
	ids := append(append([]int64(nil), r.done...), r.rejected...)
	if err := w.opt.Store.Delete(ctx, w.opt.Queue, ids); err != nil {
		return err
	}
	if n := len(r.rejected); n > 0 {
		w.opt.Logger.WarnContext(ctx, "server rejected heartbeats; dropped them",
			"worker", w.opt.Name, "count", n, "status", r.status, "err", r.err)
		if err := w.opt.Store.Add(ctx, rejectedKey(w.opt.Name), int64(n)); err != nil {
			return err
		}
		w.mu.Lock()
		w.state.Rejected += int64(n)
		w.mu.Unlock()
	}
	return nil
}

func (w *Worker) succeed(ctx context.Context, sent int) {
	now := w.opt.Now().UTC()
	w.mu.Lock()
	w.failures, w.nextTry = 0, time.Time{}
	// Grow back towards the full batch after a split.
	w.batch = min(w.batch*2, w.opt.Batch)
	w.state.LastSuccess = now
	w.state.NextAttempt = time.Time{}
	w.state.LastError, w.state.LastErrorAt = "", time.Time{}
	w.mu.Unlock()
	// Clear the persisted error too, including one left by an earlier run,
	// so status doesn't show a problem that is already fixed.
	w.persist(ctx,
		localdb.KV{Key: stateKey(w.opt.Name, keyLastSuccess), Value: now.Format(time.RFC3339)},
		localdb.KV{Key: stateKey(w.opt.Name, keyLastError), Value: ""},
		localdb.KV{Key: stateKey(w.opt.Name, keyLastErrorAt), Value: ""})
	w.opt.Logger.DebugContext(ctx, "uploaded heartbeats", "worker", w.opt.Name, "count", sent)
}

func (w *Worker) fail(ctx context.Context, r result) time.Duration {
	now := w.opt.Now().UTC()
	w.mu.Lock()
	w.failures++
	wait := Backoff(w.failures, w.opt.MinBackoff, w.opt.MaxBackoff, w.opt.Jitter())
	if r.retryAfter > wait {
		wait = min(r.retryAfter, maxRetryAfter)
	}
	w.nextTry = now.Add(wait)
	w.state.LastError, w.state.LastErrorAt, w.state.NextAttempt = r.err.Error(), now, w.nextTry
	failures := w.failures
	w.mu.Unlock()
	w.opt.Logger.WarnContext(ctx, "upload failed; backing off", "worker", w.opt.Name,
		"err", r.err, "attempt", failures, "wait", wait.Round(time.Second))
	w.persist(ctx,
		localdb.KV{Key: stateKey(w.opt.Name, keyLastError), Value: r.err.Error()},
		localdb.KV{Key: stateKey(w.opt.Name, keyLastErrorAt), Value: now.Format(time.RFC3339)})
	return wait
}

func (w *Worker) pause(ctx context.Context, key string, cause error) {
	now := w.opt.Now().UTC()
	w.mu.Lock()
	w.paused, w.pausedKey, w.probeAt = true, key, w.opt.Now().Add(w.opt.Reprobe)
	w.state.LastError, w.state.LastErrorAt, w.state.Paused = cause.Error(), now, ReasonReLogin
	w.mu.Unlock()
	w.opt.Logger.ErrorContext(ctx, "upload key rejected; paused until the key changes",
		"worker", w.opt.Name, "retry_in", w.opt.Reprobe)
	w.persist(ctx,
		localdb.KV{Key: stateKey(w.opt.Name, keyPaused), Value: ReasonReLogin},
		localdb.KV{Key: stateKey(w.opt.Name, keyLastError), Value: cause.Error()},
		localdb.KV{Key: stateKey(w.opt.Name, keyLastErrorAt), Value: now.Format(time.RFC3339)})
}

func (w *Worker) persist(ctx context.Context, kvs ...localdb.KV) {
	if err := w.opt.Store.SetMany(ctx, kvs...); err != nil {
		w.opt.Logger.WarnContext(ctx, "saving upload state", "worker", w.opt.Name, "err", err)
	}
}

// Backoff returns the wait after the given number of consecutive failures:
// min doubled per failure, capped at max, with "equal jitter" (half fixed,
// half random). jitter must be in [0, 1).
func Backoff(failures int, minWait, maxWait time.Duration, jitter float64) time.Duration {
	d := minWait
	for i := 1; i < failures && d < maxWait; i++ {
		d *= 2
	}
	d = min(d, maxWait)
	return d/2 + time.Duration(jitter*float64(d/2))
}

// ReasonReLogin is the pause reason after the server rejects the key.
const ReasonReLogin = "re-login needed"
