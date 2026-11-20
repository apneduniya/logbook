package activity

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"time"

	"github.com/apneduniya/logbook/internal/agent/sensors/focusclass"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Defaults for the sampling cadence (ADR 0007).
const (
	DefaultInterval      = 5 * time.Second
	DefaultSlowInterval  = 30 * time.Second
	DefaultIdleThreshold = 10 * time.Minute
	probeTimeout         = 2 * time.Second
)

// Sink receives samples. It has the same method set as the daemon's Sink, so
// a daemon.Sink value satisfies it without this package importing daemon.
// It is an alias of an unnamed interface type so that, if the daemon declares
// its Sink the same way, Run's signature matches daemon.Sensor exactly.
type Sink = interface {
	RecordActivity(ctx context.Context, s wire.ActivitySample) error
	RecordGit(ctx context.Context, g wire.GitEvent) error
}

// Probe reads the OS. osprobe.Probe implements it; tests use fakes.
type Probe interface {
	// IdleTime returns the time since the last user input, or an error
	// when it is unknown.
	IdleTime(ctx context.Context) (time.Duration, error)
	// FocusedApp returns the focused app's process name or bundle id, "" if unknown.
	FocusedApp(ctx context.Context) (string, error)
	// ScreenLocked reports whether the screen is locked.
	ScreenLocked(ctx context.Context) (bool, error)
}

// Classifier maps an app name to a focus kind.
type Classifier interface {
	Classify(app string) focusclass.Kind
}

// Sensor samples activity. Create it with New; Run it once.
type Sensor struct {
	probe         Probe
	classifier    Classifier
	log           *slog.Logger
	now           func() time.Time
	sleep         func(ctx context.Context, d time.Duration) error
	interval      time.Duration
	slowInterval  time.Duration
	idleThreshold time.Duration

	// failing remembers which calls are currently failing, so each failure
	// streak is logged once instead of every 5 seconds.
	failing map[string]bool
}

// Option configures a Sensor.
type Option func(*Sensor)

// WithLogger sets the logger (default: discard).
func WithLogger(l *slog.Logger) Option { return func(s *Sensor) { s.log = l } }

// WithClassifier replaces the focus classifier (default: focusclass for runtime.GOOS).
func WithClassifier(c Classifier) Option { return func(s *Sensor) { s.classifier = c } }

// WithClock sets the time source used to stamp samples.
func WithClock(now func() time.Time) Option { return func(s *Sensor) { s.now = now } }

// WithSleep replaces the wait between samples; it must return ctx.Err() when
// ctx is done. Tests use it to observe the cadence without real time.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(s *Sensor) { s.sleep = sleep }
}

// WithIntervals sets the normal and slow sampling intervals.
func WithIntervals(normal, slow time.Duration) Option {
	return func(s *Sensor) { s.interval, s.slowInterval = normal, slow }
}

// WithIdleThreshold sets the idle time after which sampling slows down.
func WithIdleThreshold(d time.Duration) Option { return func(s *Sensor) { s.idleThreshold = d } }

// New returns an activity sensor reading from probe.
func New(probe Probe, opts ...Option) *Sensor {
	s := &Sensor{
		probe:         probe,
		classifier:    focusclass.New(runtime.GOOS),
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:           time.Now,
		sleep:         sleepCtx,
		interval:      DefaultInterval,
		slowInterval:  DefaultSlowInterval,
		idleThreshold: DefaultIdleThreshold,
		failing:       map[string]bool{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Name identifies the sensor in daemon logs and status.
func (s *Sensor) Name() string { return "activity" }

// Run samples until ctx is done, then returns nil. Probe and sink errors are
// logged and never stop the sensor.
func (s *Sensor) Run(ctx context.Context, sink Sink) error {
	for {
		wait := s.step(ctx, sink)
		if err := s.sleep(ctx, wait); err != nil {
			return nil //nolint:nilerr // ctx cancellation is the normal way to stop
		}
	}
}

// step takes one sample, records it and returns how long to wait.
func (s *Sensor) step(ctx context.Context, sink Sink) time.Duration {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	locked, err := s.probe.ScreenLocked(pctx)
	s.track("lock", err)
	if err != nil {
		locked = false
	}

	idle, err := s.probe.IdleTime(pctx)
	s.track("idle", err)
	if err != nil {
		return s.slowInterval // unknown: emit nothing (see package doc)
	}
	idle = max(idle, 0)

	app := ""
	if !locked {
		app, err = s.probe.FocusedApp(pctx)
		s.track("focus", err)
		if err != nil {
			app = ""
		}
	}
	kind := s.classifier.Classify(app)
	if app == "" {
		kind = focusclass.Unknown
	}

	sample := wire.ActivitySample{
		Time:        s.now().UTC(),
		IdleSeconds: idle.Round(100 * time.Millisecond).Seconds(),
		FocusApp:    app,
		FocusKind:   string(kind),
	}
	s.track("sink", sink.RecordActivity(ctx, sample))

	if locked || idle >= s.idleThreshold {
		return s.slowInterval
	}
	return s.interval
}

// track logs the first error of a streak and the recovery after it.
func (s *Sensor) track(what string, err error) {
	switch {
	case err != nil && !s.failing[what]:
		s.failing[what] = true
		s.log.Warn("activity sensor call failing", "call", what, "err", err)
	case err == nil && s.failing[what]:
		delete(s.failing, what)
		s.log.Info("activity sensor call recovered", "call", what)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
