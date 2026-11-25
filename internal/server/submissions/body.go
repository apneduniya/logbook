package submissions

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// uploadReadTimeout is how long an upload's body may take to arrive: 32 MiB
// at 1 Mbit/s, with room to spare.
const uploadReadTimeout = 10 * time.Minute

// errDecompressedTooLarge is returned by capReader past its limit.
var errDecompressedTooLarge = errors.New("decompressed body too large")

// capReader reads at most left bytes of r and fails with
// errDecompressedTooLarge if r has more.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		if n, err := c.r.Read(one[:]); n > 0 {
			return 0, errDecompressedTooLarge
		} else if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// Upload concurrency (docs/api/submissions.md). Decoding a body holds
// several times its decompressed size in memory (the JSON decoder's buffer,
// the decoded strings, then the part re-encoded for storage): about 1.3 GB
// for a body at the 256 MiB limit. So each user uploads one body at a time,
// a body is read in full (compressed, at most 32 MiB) before it waits for
// one of defaultUploadSlots decode slots, and a body that waits longer than
// defaultUploadWait is refused with 503.
const (
	defaultUploadSlots = 2
	defaultUploadWait  = time.Minute
	// uploadRetryAfter is the Retry-After of a 429 or 503 from the gate, in
	// seconds.
	uploadRetryAfter = "60"
)

// uploadGate bounds how many upload bodies are in memory at once.
type uploadGate struct {
	slots chan struct{}
	wait  time.Duration

	mu   sync.Mutex
	busy map[uuid.UUID]bool
}

func newUploadGate(slots int, wait time.Duration) *uploadGate {
	if slots <= 0 {
		slots = defaultUploadSlots
	}
	if wait <= 0 {
		wait = defaultUploadWait
	}
	return &uploadGate{slots: make(chan struct{}, slots), wait: wait, busy: map[uuid.UUID]bool{}}
}

// enter claims the user's one upload in progress: 429 while another runs.
// The returned func releases it.
func (g *uploadGate) enter(w http.ResponseWriter, user uuid.UUID) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[user] {
		w.Header().Set("Retry-After", uploadRetryAfter)
		return nil, httpapi.Errorf(http.StatusTooManyRequests, nil, "another upload of yours is still in progress; try again when it ends")
	}
	g.busy[user] = true
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.busy, user)
	}, nil
}

// slot waits up to g.wait for a decode slot: 503 when none frees up (or
// the request ends first). The returned func releases it.
func (g *uploadGate) slot(ctx context.Context, w http.ResponseWriter) (func(), error) {
	t := time.NewTimer(g.wait)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, nil
	case <-t.C:
	case <-ctx.Done():
	}
	w.Header().Set("Retry-After", uploadRetryAfter)
	return nil, httpapi.Errorf(http.StatusServiceUnavailable, ctx.Err(), "the server is busy with other uploads; try again in a minute")
}

// readGzipBody reads a gzip-compressed body in full: it must say
// Content-Encoding: gzip (415 otherwise) and be at most maxGzip bytes as
// sent (413). The server's read timeout is extended to uploadReadTimeout,
// so a large bundle on a slow link isn't cut off.
func readGzipBody(w http.ResponseWriter, r *http.Request, maxGzip int64) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		return nil, httpapi.Errorf(http.StatusUnsupportedMediaType, nil, "send the body gzip-compressed, with Content-Encoding: gzip")
	}
	// Writers that can't set deadlines (tests) keep their own.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxGzip)); err != nil {
		return nil, bodyError(err, "reading the body")
	}
	return buf.Bytes(), nil
}

// decodeGzipJSON decodes gzip-compressed JSON into v: at most maxJSON bytes
// decompressed (413), and one JSON value with an intact gzip trailer (400).
func decodeGzipJSON(gz []byte, maxJSON int64, v any) error {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return bodyError(err, "the body isn't gzip")
	}
	defer func() { _ = zr.Close() }() // closing a gzip reader only releases it
	dec := json.NewDecoder(&capReader{r: zr, left: maxJSON})
	if err := dec.Decode(v); err != nil {
		return bodyError(err, "invalid JSON body")
	}
	// Reading to the end checks the gzip checksum and that nothing follows.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return httpapi.Errorf(http.StatusBadRequest, nil, "unexpected data after JSON body")
		}
		return bodyError(err, "unexpected data after JSON body")
	}
	return nil
}

func bodyError(err error, msg string) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) || errors.Is(err, errDecompressedTooLarge) {
		return httpapi.Errorf(http.StatusRequestEntityTooLarge, err, "request body too large")
	}
	return httpapi.Errorf(http.StatusBadRequest, err, "%s", msg)
}
