package submissions

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

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

// decodeGzipJSON decodes a gzip-compressed JSON body into v while it
// streams: the body must say Content-Encoding: gzip (415 otherwise), is at
// most maxGzip bytes as sent and maxJSON bytes decompressed (413), and must
// be one JSON value with an intact gzip trailer (400). The server's read
// timeout is extended to uploadReadTimeout, so a large bundle on a slow
// link isn't cut off.
func decodeGzipJSON(w http.ResponseWriter, r *http.Request, maxGzip, maxJSON int64, v any) error {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		return httpapi.Errorf(http.StatusUnsupportedMediaType, nil, "send the body gzip-compressed, with Content-Encoding: gzip")
	}
	// Writers that can't set deadlines (tests) keep their own.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	body := http.MaxBytesReader(w, r.Body, maxGzip)
	zr, err := gzip.NewReader(body)
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
