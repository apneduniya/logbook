package upload

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
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
	if cfg.APIKey == "" {
		t.Disabled = "not signed in; run `logbook login`"
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
	if !MirrorEnabled(cfg) {
		t.Disabled = "mirroring to Hackatime is off"
	}
	return t
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
	Batch                            int
	Now                              func() time.Time
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
	// until the configured key differs from it.
	pausedKey string
	paused    bool
	state     State
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
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Worker{opt: o, kick: make(chan struct{}, 1), state: State{Name: o.Name}}
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
		items, err := w.opt.Store.Peek(ctx, w.opt.Queue, w.opt.Batch)
		if err != nil {
			w.opt.Logger.ErrorContext(ctx, "reading upload queue", "worker", w.opt.Name, "err", err)
			return w.opt.Interval
		}
		if len(items) == 0 {
			return w.opt.Interval
		}
		r := send(ctx, w.opt.Client, w.opt.UserAgent, t, items)
		if err := w.apply(ctx, r); err != nil {
			w.opt.Logger.ErrorContext(ctx, "updating upload queue", "worker", w.opt.Name, "err", err)
			return w.opt.Interval
		}
		switch {
		case r.outcome == outcomeAuth:
			w.pause(ctx, t.APIKey, r.err)
			return w.opt.Interval
		case r.outcome == outcomeRetry || r.retry > 0:
			return w.fail(ctx, r)
		}
		w.succeed(ctx, len(r.done))
		if len(items) < w.opt.Batch {
			return w.opt.Interval
		}
	}
	return 0 // more is waiting; come straight back
}

// ready reports whether the worker may send to t, updating its pause state.
func (w *Worker) ready(ctx context.Context, t Target) bool {
	w.mu.Lock()
	ok := true
	reason := ""
	switch {
	case t.Disabled != "":
		ok, reason = false, t.Disabled
	case w.paused && t.APIKey == w.pausedKey:
		ok, reason = false, ReasonReLogin
	case w.paused:
		// The key changed (e.g. after `logbook login`): try again.
		w.paused, w.pausedKey = false, ""
		w.failures, w.nextTry = 0, time.Time{}
	}
	changed := w.state.Paused != reason
	w.state.Paused = reason
	w.mu.Unlock()
	if changed {
		w.persist(ctx, localdb.KV{Key: stateKey(w.opt.Name, keyPaused), Value: reason})
	}
	return ok
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
	w.state.LastSuccess = now
	w.state.NextAttempt = time.Time{}
	w.mu.Unlock()
	w.persist(ctx, localdb.KV{Key: stateKey(w.opt.Name, keyLastSuccess), Value: now.Format(time.RFC3339)})
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
	w.paused, w.pausedKey = true, key
	w.state.LastError, w.state.LastErrorAt, w.state.Paused = cause.Error(), now, ReasonReLogin
	w.mu.Unlock()
	w.opt.Logger.ErrorContext(ctx, "upload key rejected; paused until the key changes", "worker", w.opt.Name)
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
