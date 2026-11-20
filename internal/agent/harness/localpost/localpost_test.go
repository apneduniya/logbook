package localpost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func events(n int) []wire.LocalEvent {
	out := make([]wire.LocalEvent, n)
	for i := range out {
		ev := wire.AIEvent{Time: time.Date(2026, 11, 20, 10, 0, i, 0, time.UTC), Harness: wire.HarnessCodex,
			Kind: wire.AITurnEnd, SessionID: "s"}
		out[i] = wire.LocalEvent{Type: "ai", AI: &ev}
	}
	return out
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSender(t *testing.T, addr string) (*Sender, string) {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "spool", "events.jsonl")
	return New(addr, token, spool, quietLog()), spool
}

func spoolLines(t *testing.T, path string) []wire.LocalEvent {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test path
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []wire.LocalEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e wire.LocalEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("spool line %q: %v", sc.Text(), err)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("spooled event invalid: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestSendToDaemon(t *testing.T) {
	var got []wire.LocalEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != EventsPath || r.Header.Get(TokenHeader) != token ||
			r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	s, spool := newSender(t, strings.TrimPrefix(srv.URL, "http://"))
	if err := s.Send(context.Background(), events(2)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("daemon got %d events", len(got))
	}
	if lines := spoolLines(t, spool); lines != nil {
		t.Fatalf("spooled %d events although the daemon took them", len(lines))
	}
}

func TestSendSpoolsOnFailure(t *testing.T) {
	closed := func() string {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}
	status := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	slow := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	cases := map[string]string{
		"daemon down":   closed(),
		"wrong token":   status(http.StatusUnauthorized),
		"daemon error":  status(http.StatusInternalServerError),
		"daemon slow":   slow(),
		"not installed": "",
	}
	for name, addr := range cases {
		t.Run(name, func(t *testing.T) {
			s, spool := newSender(t, addr)
			start := time.Now()
			if err := s.Send(context.Background(), events(3)); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > DefaultTimeout+700*time.Millisecond {
				t.Fatalf("Send took %v", d)
			}
			if n := len(spoolLines(t, spool)); n != 3 {
				t.Fatalf("spooled %d events, want 3", n)
			}
			if runtimeNotWindows() {
				st, err := os.Stat(spool)
				if err != nil || st.Mode().Perm() != 0o600 {
					t.Fatalf("spool mode %v, %v", st.Mode(), err)
				}
				dst, _ := os.Stat(filepath.Dir(spool))
				if dst.Mode().Perm() != 0o700 {
					t.Fatalf("spool dir mode %v", dst.Mode())
				}
			}
		})
	}
}

func TestSpoolCap(t *testing.T) {
	s, spool := newSender(t, "")
	s.MaxSpool = 600
	if err := s.Send(context.Background(), events(2)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(spool) //nolint:gosec // test path
	err := s.Send(context.Background(), events(5))
	if !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("Send over cap = %v, want ErrSpoolFull", err)
	}
	after, _ := os.ReadFile(spool) //nolint:gosec // test path
	if string(before) != string(after) {
		t.Fatal("refused write changed the spool")
	}
}

// Concurrent hook processes append whole lines that never interleave.
func TestConcurrentSpool(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "events.jsonl")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			s := New("", "", spool, quietLog())
			if err := s.Send(context.Background(), events(5)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := len(spoolLines(t, spool)); n != 100 {
		t.Fatalf("spool has %d events, want 100", n)
	}
}

func TestSendNothing(t *testing.T) {
	s, spool := newSender(t, "")
	if err := s.Send(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty send created a spool")
	}
}
