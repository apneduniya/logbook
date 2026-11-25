package admin_test

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/admin"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/submissions"
)

// Bundle exports are limited server-wide: each holds a stored part in
// memory while its client reads, so one past the limit is refused with
// 503 and Retry-After instead of waiting, and is audited.
func TestBundleExportsAreLimited(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	ev := &slowBundles{started: make(chan struct{}, 10), release: make(chan struct{})}
	released := false
	release := func() {
		if !released {
			released = true
			close(ev.release)
		}
	}
	defer release()
	g := *f
	g.mux = http.NewServeMux()
	reads := oneSubmission{Store: f.st, id: id, user: f.orpheus}
	admin.NewHandler(admin.NewService(reads, ev, nil), f.keys, admin.HandlerOptions{BundleExports: 1}).Register(g.mux)
	target := "/api/admin/v1/submissions/" + id.String() + "/bundle"

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- g.do(t, call{target: target}) }()
	select {
	case <-ev.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the first export didn't start")
	}

	before := len(audit(t, f.st, "admin.read"))
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- g.do(t, call{target: target}) }()
	select {
	case rec := <-second:
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("second export = %d %s", rec.Code, rec.Body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second export wasn't refused while the first ran")
	}
	if rows := audit(t, f.st, "admin.read")[before:]; len(rows) != 1 || rows[0].Data["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("refused export audited as %+v", rows)
	}

	release()
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("first export = %d", rec.Code)
	}
	// The slot is free again.
	if rec := g.do(t, call{target: target}); rec.Code != http.StatusOK {
		t.Fatalf("export after the first = %d %s", rec.Code, rec.Body)
	}
}

// oneSubmission is the store with one more submission, of user.
type oneSubmission struct {
	*store.Store
	id, user uuid.UUID
}

func (o oneSubmission) AdminSubmissions(ctx context.Context, arg db.AdminSubmissionsParams) ([]db.AdminSubmissionsRow, error) {
	if arg.ID.Valid && arg.ID.Bytes == o.id {
		return []db.AdminSubmissionsRow{{ID: o.id, UserID: o.user}}, nil
	}
	return o.Store.AdminSubmissions(ctx, arg)
}

// slowBundles exports a bundle only once release is closed.
type slowBundles struct {
	started chan struct{}
	release chan struct{}
}

func (s *slowBundles) CheckBundle(context.Context, uuid.UUID) error { return nil }

func (s *slowBundles) WriteBundle(_ context.Context, _ uuid.UUID, w io.Writer) error {
	s.started <- struct{}{}
	<-s.release
	_, err := io.WriteString(w, "{}")
	return err
}

func (s *slowBundles) PartRows(context.Context, uuid.UUID, uuid.UUID, int, int) ([]submissions.EvidenceRow, int, error) {
	return nil, 0, nil
}

// A client that stops reading a bundle download loses its export slot once
// a write waits longer than the write timeout, instead of holding it for
// as long as its connection stays open.
func TestStalledExportFreesItsSlot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	g := *f
	g.mux = http.NewServeMux()
	reads := oneSubmission{Store: f.st, id: id, user: f.orpheus}
	admin.NewHandler(admin.NewService(reads, bigBundles{}, nil), f.keys,
		admin.HandlerOptions{BundleExports: 1, ExportWriteTimeout: 200 * time.Millisecond}).Register(g.mux)
	srv := httptest.NewServer(g.mux)
	defer srv.Close()
	target := "/api/admin/v1/submissions/" + id.String() + "/bundle"

	// The stalled client reads the status line, then nothing more.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", target, f.key)
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("stalled client got %q, %v", line, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+target, nil)
		r.Header.Set("Authorization", "Bearer "+f.key)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("export still %d while the stalled client holds its connection", resp.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// bigBundles writes 64 MiB of incompressible bytes, more than socket
// buffers hold, so a client that stops reading blocks the writer.
type bigBundles struct{}

func (bigBundles) CheckBundle(context.Context, uuid.UUID) error { return nil }

func (bigBundles) WriteBundle(_ context.Context, _ uuid.UUID, w io.Writer) error {
	_, err := io.CopyN(w, cryptorand.Reader, 64<<20)
	return err
}

func (bigBundles) PartRows(context.Context, uuid.UUID, uuid.UUID, int, int) ([]submissions.EvidenceRow, int, error) {
	return nil, 0, nil
}

// An export that fails after its submission is found (raw rows deleted,
// or no free slot) is audited against the submission's owner, not only
// the key.
func TestFailedExportAuditedAgainstOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	g := *f
	g.mux = http.NewServeMux()
	reads := oneSubmission{Store: f.st, id: id, user: f.orpheus}
	admin.NewHandler(admin.NewService(reads, deletedBundles{}, nil), f.keys, admin.HandlerOptions{}).Register(g.mux)
	before := len(audit(t, f.st, "admin.read"))
	g.expect(t, call{target: "/api/admin/v1/submissions/" + id.String() + "/bundle"}, http.StatusGone)
	rows := audit(t, f.st, "admin.read")[before:]
	if len(rows) != 1 || rows[0].SubjectType != "user" || rows[0].SubjectID != f.orpheus.String() ||
		rows[0].Data["status"] != float64(http.StatusGone) {
		t.Fatalf("failed export audited as %+v", rows)
	}
}

// deletedBundles answers every export as deleted.
type deletedBundles struct{ bigBundles }

func (deletedBundles) CheckBundle(context.Context, uuid.UUID) error {
	return submissions.ErrEvidenceDeleted
}
