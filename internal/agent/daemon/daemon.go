package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localapi"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/localkey"
	"github.com/apneduniya/logbook/internal/agent/otlp"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/statuspage"
	"github.com/apneduniya/logbook/internal/agent/upload"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Sink is where sensors record what they observe. The daemon passes its
// local database.
type Sink interface {
	RecordActivity(ctx context.Context, s wire.ActivitySample) error
	RecordGit(ctx context.Context, g wire.GitEvent) error
}

// Sensor is a long-running observer. Run blocks until ctx is done. If it
// returns early or panics, the daemon restarts it with backoff.
type Sensor interface {
	Name() string
	Run(ctx context.Context, sink Sink) error
}

// Repo is a git repository with recent activity.
type Repo struct{ Path, Project string }

// RepoSource lists repositories with activity in the last hour.
type RepoSource interface {
	ActiveRepos(ctx context.Context) ([]Repo, error)
}

// Intervals for the daemon's housekeeping.
const (
	spoolEvery      = time.Minute
	pruneEvery      = 6 * time.Hour
	spoolGrace      = 2 * time.Second
	httpTimeout     = 30 * time.Second
	probeTimeout    = time.Second
	housekeepingMax = 2 * time.Minute
)

// ErrAlreadyRunning means another agent already serves the local address.
var ErrAlreadyRunning = errors.New("the logbook agent is already running")

// Options configures the daemon.
type Options struct {
	DataDir string
	Config  config.Config
	// Sensors run for the daemon's lifetime.
	Sensors []Sensor
	// RepoSensors build sensors that need the active-repo list (the git sensor).
	RepoSensors []func(RepoSource) Sensor
	// Harnesses report AI hook installation for the status page; nil shows "unknown".
	Harnesses []status.Harness
	Logger    *slog.Logger
	Version   string
	UserAgent string

	// Test knobs; zero means the production default.
	uploadInterval time.Duration
	restartFloor   time.Duration
	spoolGrace     time.Duration
	settingsEvery  time.Duration
}

// Run runs the daemon until ctx is done, then shuts down gracefully:
// in-flight requests finish, uploaders make a last bounded attempt, and the
// database is closed. It returns ErrAlreadyRunning if another agent holds
// the local address.
func Run(ctx context.Context, o Options) error {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	log := o.Logger
	ln, err := listen(ctx, o.Config.LocalAddr)
	if err != nil {
		return err
	}
	// The listener is the single-instance lock; it's closed by the API's
	// shutdown, or here if start-up fails before that.
	served := false
	defer func() {
		if !served {
			_ = ln.Close()
		}
	}()

	db, err := localdb.Open(ctx, config.DBPath(o.DataDir), localdb.WithLogger(log))
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("closing database", "err", err)
		}
	}()

	set := newSettings(o.DataDir, o.Config, log, o.settingsEvery)
	client := NewHTTPClient()
	page, err := statuspage.New(nil)
	if err != nil {
		return err
	}

	workers := []*upload.Worker{
		upload.New(upload.Options{
			Name: status.NameLogbook, Queue: localdb.HeartbeatQueue, Store: db, Client: client,
			UserAgent: o.UserAgent, Logger: log, Interval: o.uploadInterval,
			Target: func() upload.Target { return upload.LogbookTarget(set.Get()) },
		}),
		upload.New(upload.Options{
			Name: status.NameHackatime, Queue: localdb.MirrorQueue, Store: db, Client: client,
			UserAgent: o.UserAgent, Logger: log, Interval: o.uploadInterval,
			Target: func() upload.Target { return upload.HackatimeTarget(set.Get()) },
		}),
	}
	started := time.Now()
	snapshot := func(ctx context.Context) (status.Snapshot, error) {
		s, err := status.Collect(ctx, status.Input{
			Store: db, Config: set.Get(), DataDir: o.DataDir, Version: o.Version,
			Harnesses: o.Harnesses, Now: time.Now(),
		})
		if err != nil {
			return status.Snapshot{}, err
		}
		s.DaemonRunning, s.StartedAt = true, started.UTC()
		s.Logbook.Worker.NextAttempt = workers[0].State().NextAttempt
		s.Hackatime.Worker.NextAttempt = workers[1].State().NextAttempt
		return s, nil
	}
	activeTime := otlp.New(otlp.Options{Store: db, Token: otlpToken(set.Get), Logger: log})
	api := localapi.New(localapi.Options{
		Settings: set.Get, Store: db, Status: snapshot, Page: page, Client: client,
		UserAgent: o.UserAgent, Logger: log, MirrorEnabled: upload.MirrorEnabled,
		OTLP: activeTime.Handler(),
		OnEnqueue: func() {
			for _, w := range workers {
				w.Kick()
			}
		},
	})
	grace := o.spoolGrace
	if grace == 0 {
		grace = spoolGrace
	}
	spool := NewSpool(config.SpoolPath(o.DataDir), db, log, grace)
	restartFloor := o.restartFloor
	if restartFloor == 0 {
		restartFloor = restartDefaultFloor
	}

	sensors := append([]Sensor(nil), o.Sensors...)
	repos := NewRepos(db, nil)
	for _, build := range o.RepoSensors {
		sensors = append(sensors, build(repos))
	}

	log.Info("logbook agent started", "version", o.Version, "addr", ln.Addr().String(), "sensors", len(sensors))
	g, gctx := errgroup.WithContext(ctx)
	served = true
	g.Go(func() error { return api.Serve(gctx, ln) })
	for _, w := range workers {
		g.Go(func() error { return w.Run(gctx) })
	}
	g.Go(func() error {
		return every(gctx, spoolEvery, func(ctx context.Context) { drainSpool(ctx, log, spool) })
	})
	g.Go(func() error { return every(gctx, pruneEvery, func(ctx context.Context) { prune(ctx, log, db) }) })
	for _, s := range sensors {
		g.Go(func() error {
			supervise(gctx, log, "sensor "+s.Name(), restartFloor, func(ctx context.Context) error { return s.Run(ctx, db) })
			return nil
		})
	}
	err = g.Wait()
	log.Info("logbook agent stopped")
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// otlpToken returns the token Claude Code's OTLP exporter must send, derived
// from the current config's local token ("" without one, which refuses every
// export).
func otlpToken(cfg func() config.Config) func() string {
	return func() string {
		tok, err := localkey.OTLPToken(cfg().LocalToken)
		if err != nil {
			return ""
		}
		return tok
	}
}

func drainSpool(ctx context.Context, log *slog.Logger, s *Spool) {
	ctx, cancel := context.WithTimeout(ctx, housekeepingMax)
	defer cancel()
	r, err := s.Drain(ctx)
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "draining hook spool", "err", err)
	}
	if r.Stored > 0 {
		log.InfoContext(ctx, "drained hook spool", "events", r.Stored)
	}
}

func prune(ctx context.Context, log *slog.Logger, db *localdb.DB) {
	ctx, cancel := context.WithTimeout(ctx, housekeepingMax)
	defer cancel()
	r, err := db.Prune(ctx)
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "pruning old local events", "err", err)
		return
	}
	if r != (localdb.PruneResult{}) {
		log.InfoContext(ctx, "pruned old local events", "ai", r.AIEvents, "activity", r.ActivitySamples,
			"git", r.GitEvents, "heartbeat_log", r.HeartbeatLog, "active_time", r.ActiveTime)
	}
}

// LoopbackAddr checks that addr is a loopback host and port and returns the
// address to listen on. "localhost" is pinned to 127.0.0.1 so a hosts-file
// entry can't move the listener.
func LoopbackAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("local_addr %q: %w", addr, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return "", fmt.Errorf("local_addr %q has no valid port", addr)
	}
	if host != "127.0.0.1" && host != "localhost" {
		return "", fmt.Errorf("local_addr %q must be 127.0.0.1:<port> or localhost:<port>", addr)
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}

// listen binds the local address. If it's taken, it checks whether another
// agent answers there.
func listen(ctx context.Context, addr string) (net.Listener, error) {
	a, err := LoopbackAddr(addr)
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", a)
	if err == nil {
		return ln, nil
	}
	if Probe(ctx, a) {
		return nil, ErrAlreadyRunning
	}
	return nil, fmt.Errorf("listening on %s (is another program using the port?): %w", a, err)
}

// Probe reports whether a logbook agent answers at addr.
func Probe(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/local/v1/status", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// NewHTTPClient returns the client the daemon shares across every outbound
// call; CLI commands use one too. It has timeouts on every phase and honours
// HTTP(S)_PROXY, which school networks often need.
//
// Redirects are followed only for GET and HEAD. Go turns a POST that gets a
// 301/302/303 into a GET, so a captive portal's redirect to its login page
// would otherwise come back as "200 OK" for an upload; the uploader sees the
// 3xx instead and keeps the batch.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       httpTimeout,
		CheckRedirect: checkRedirect,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// maxRedirects matches net/http's default limit.
const maxRedirects = 10

// checkRedirect follows redirects only when the original request was a GET
// or HEAD; for anything else the 3xx response is returned as is.
func checkRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) > 0 && via[0].Method != http.MethodGet && via[0].Method != http.MethodHead {
		return http.ErrUseLastResponse
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}
