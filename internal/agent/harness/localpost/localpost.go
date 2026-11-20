package localpost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Defaults from the shared hook contract.
const (
	// DefaultTimeout bounds the whole POST to the daemon.
	DefaultTimeout = 300 * time.Millisecond
	// DefaultMaxSpool is the spool file's size cap.
	DefaultMaxSpool = 10 << 20
	// EventsPath is the daemon's intake endpoint.
	EventsPath = "/local/v1/events"
	// TokenHeader carries the local token.
	TokenHeader = "X-Logbook-Token" //nolint:gosec // a header name, not a credential
)

// ErrSpoolFull means the spool file reached its cap and the events were
// dropped.
var ErrSpoolFull = errors.New("spool file is full")

// Sender posts events to the daemon and spools them when it can't.
type Sender struct {
	// Addr is the daemon's host:port ("" skips straight to the spool).
	Addr string
	// Token is the local token. It is never logged.
	Token string
	// SpoolPath is the spool file (config.SpoolPath).
	SpoolPath string
	// MaxSpool caps the spool file size in bytes.
	MaxSpool int64
	// Client sends the request. Its own timeout, if any, also applies.
	Client *http.Client
	// Timeout bounds the POST.
	Timeout time.Duration
	// Log records spool problems. It must not write to the hook's stdout or
	// stderr, which harnesses read.
	Log *slog.Logger
}

// New returns a Sender with the contract's defaults and an HTTP client that
// never uses a proxy (the daemon is on loopback).
func New(addr, token, spoolPath string, log *slog.Logger) *Sender {
	return &Sender{
		Addr: addr, Token: token, SpoolPath: spoolPath,
		MaxSpool: DefaultMaxSpool, Timeout: DefaultTimeout, Log: log,
		Client: &http.Client{Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		}},
	}
}

// Send delivers events: to the daemon if it answers 2xx in time, else to the
// spool. It returns an error only if the events were lost.
func (s *Sender) Send(ctx context.Context, events []wire.LocalEvent) error {
	if len(events) == 0 {
		return nil
	}
	if s.Addr != "" && s.Token != "" {
		addr, err := Loopback(s.Addr)
		if err == nil {
			err = s.post(ctx, addr, events)
		}
		if err == nil {
			return nil
		}
		s.Log.Debug("daemon unavailable, spooling hook events", "err", err)
	}
	if err := s.spool(events); err != nil {
		s.Log.Warn("dropping hook events", "count", len(events), "err", err)
		return err
	}
	return nil
}

// ErrNotLoopback means the configured daemon address isn't on this machine.
// The token and events go over plain HTTP, so they are spooled instead.
var ErrNotLoopback = errors.New("daemon address is not a loopback address")

// Loopback checks that addr (host:port) names this machine and returns it
// with "localhost" replaced by 127.0.0.1, so name resolution (a hosts file,
// a proxy) can't send the token elsewhere.
func Loopback(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("daemon address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: %q", ErrNotLoopback, host)
	}
	return addr, nil
}

func (s *Sender) post(ctx context.Context, addr string, events []wire.LocalEvent) error {
	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("encoding events: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+EventsPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TokenHeader, s.Token)
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("posting events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // response fully handled below
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("daemon answered %s", resp.Status)
	}
	return nil
}

// spool appends events as JSON lines in a single write, so concurrent hook
// processes never interleave partial lines (O_APPEND).
func (s *Sender) spool(events []wire.LocalEvent) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("encoding event: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.SpoolPath), 0o700); err != nil { //nolint:gosec // inside the agent's data dir
		return fmt.Errorf("creating spool dir: %w", err)
	}
	f, err := os.OpenFile(s.SpoolPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // path comes from config.SpoolPath
	if err != nil {
		return fmt.Errorf("opening spool: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat spool: %w", err)
	}
	if st.Size()+int64(buf.Len()) > s.MaxSpool {
		_ = f.Close()
		return fmt.Errorf("%w (%d bytes)", ErrSpoolFull, st.Size())
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing spool: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing spool: %w", err)
	}
	return nil
}
