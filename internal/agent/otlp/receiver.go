package otlp

import (
	"cmp"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// MaxBody is the most a request body may hold, compressed or not.
const MaxBody = 1 << 20

// TokenHeader carries the OTLP token (localkey.OTLPToken). Claude Code sends
// it from OTEL_EXPORTER_OTLP_METRICS_HEADERS.
const TokenHeader = "X-Logbook-Token" //nolint:gosec // a header name, not a credential

// Store is where active time goes. *localdb.DB satisfies it.
type Store interface {
	LastCumulative(ctx context.Context, sessionID, typ string, seriesStart time.Time) (to time.Time, total float64, ok bool, err error)
	InsertActiveTime(ctx context.Context, pts []localdb.ActiveTimePoint) error
}

// Options configures a Receiver.
type Options struct {
	Store Store
	// Token returns the token requests must carry; "" refuses every request
	// (no config yet). It is read on every request, so a new config applies
	// without a restart.
	Token  func() string
	Logger *slog.Logger
	Now    func() time.Time
}

// Receiver serves the OTLP endpoints.
type Receiver struct {
	opt Options
	// mu serialises imports, so two requests can't convert the same
	// cumulative series against the same stored point.
	mu sync.Mutex
	// warnedProto makes the "protobuf isn't supported" warning appear once.
	warnedProto atomic.Bool
}

// New returns a Receiver.
func New(o Options) *Receiver {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Receiver{opt: o}
}

// Handler returns POST /v1/metrics and POST /v1/logs. The caller adds the
// loopback and Host checks (localapi does).
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", r.metrics)
	mux.HandleFunc("POST /v1/logs", r.logs)
	return mux
}

// exportResponse is ExportMetricsServiceResponse in OTLP JSON. An empty
// object means everything was accepted.
type exportResponse struct {
	PartialSuccess *partialSuccess `json:"partialSuccess,omitempty"`
}

type partialSuccess struct {
	RejectedDataPoints string `json:"rejectedDataPoints"` // int64, a string in OTLP JSON
	ErrorMessage       string `json:"errorMessage"`
}

func (r *Receiver) metrics(w http.ResponseWriter, req *http.Request) {
	body, ok := r.readBody(w, req, true)
	if !ok {
		return
	}
	d, err := Decode(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "body is not an OTLP JSON metrics request")
		return
	}
	pts, rejected, err := r.store(req.Context(), d.Points)
	if err != nil {
		r.opt.Logger.ErrorContext(req.Context(), "storing Claude Code active time", "points", len(pts), "err", err)
		// 503 is retryable for OTLP exporters, so the data isn't lost.
		writeError(w, http.StatusServiceUnavailable, "could not store metrics")
		return
	}
	resp := exportResponse{}
	if n := d.Rejected + rejected; n > 0 {
		r.opt.Logger.WarnContext(req.Context(), "dropped unusable active-time datapoints", "count", n)
		resp.PartialSuccess = &partialSuccess{
			RejectedDataPoints: strconv.Itoa(n),
			ErrorMessage:       "datapoints without session.id or type, or with implausible times or values",
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// logs accepts and discards OTLP logs. The installer never points Claude
// Code's logs here; a user who points a generic OTLP endpoint at the agent
// gets a success instead of retries and error spam. Prompts are counted from
// hooks instead.
func (r *Receiver) logs(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.readBody(w, req, false); !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// readBody checks the token, content type and encoding and reads at most
// MaxBody bytes (after decompression too). On failure it has answered and ok
// is false. With requireJSON false the content type isn't checked.
func (r *Receiver) readBody(w http.ResponseWriter, req *http.Request, requireJSON bool) ([]byte, bool) {
	want := r.opt.Token()
	if want == "" || subtle.ConstantTimeCompare([]byte(req.Header.Get(TokenHeader)), []byte(want)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	if requireJSON {
		mt, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if mt != "application/json" {
			if mt == "application/x-protobuf" && !r.warnedProto.Swap(true) {
				r.opt.Logger.WarnContext(req.Context(),
					"Claude Code sent OTLP protobuf; Logbook reads only JSON. Set OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/json (logbook install does)")
			}
			writeError(w, http.StatusUnsupportedMediaType, "only application/json OTLP is supported")
			return nil, false
		}
	}
	var src io.Reader = http.MaxBytesReader(w, req.Body, MaxBody)
	switch enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad gzip body")
			return nil, false
		}
		defer func() { _ = zr.Close() }() // reading only; nothing to flush
		src = zr
	default:
		writeError(w, http.StatusUnsupportedMediaType, "unsupported Content-Encoding")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(src, MaxBody+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || len(body) > MaxBody:
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	case err != nil:
		writeError(w, http.StatusBadRequest, "could not read body")
		return nil, false
	}
	return body, true
}

// seriesKey identifies a cumulative series.
type seriesKey struct {
	session, typ string
	start        time.Time
}

// store converts points to stored increments and saves them. Points are
// processed in a fixed order so the result never depends on the order
// Claude Code listed them in.
func (r *Receiver) store(ctx context.Context, pts []Point) (stored []localdb.ActiveTimePoint, rejected int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pts = slices.Clone(pts)
	slices.SortFunc(pts, func(a, b Point) int {
		return cmp.Or(cmp.Compare(a.SessionID, b.SessionID), cmp.Compare(a.Type, b.Type),
			a.Start.Compare(b.Start), a.Time.Compare(b.Time), cmp.Compare(a.Value, b.Value))
	})
	now := r.opt.Now()
	lasts := map[seriesKey]Last{}
	for _, p := range pts {
		row := localdb.ActiveTimePoint{SessionID: p.SessionID, Type: p.Type, Repository: p.Repository}
		var (
			iv Interval
			ok bool
		)
		if p.Cumulative {
			k := seriesKey{p.SessionID, p.Type, p.Start}
			last, has := lasts[k]
			if !has {
				to, total, found, err := r.opt.Store.LastCumulative(ctx, p.SessionID, p.Type, p.Start)
				if err != nil {
					return nil, 0, fmt.Errorf("reading series state: %w", err)
				}
				last, has = Last{To: to, Total: total}, found
			}
			if iv, ok = CumulativeInterval(p, last, has, now); ok {
				total := p.Value
				row.SeriesStart, row.Cumulative = p.Start, &total
				lasts[k] = Last{To: p.Time, Total: p.Value}
			} else if has && !p.Time.After(last.To) {
				continue // a retried point: already counted, not an error
			}
		} else {
			iv, ok = DeltaInterval(p, now)
			if !ok && p.Value == 0 && plausible(Interval{From: p.Time, To: p.Time}, now) {
				continue // nothing happened in this interval
			}
		}
		if !ok {
			rejected++
			continue
		}
		row.From, row.To, row.Secs = iv.From, iv.To, iv.Secs
		stored = append(stored, row)
	}
	if err := r.opt.Store.InsertActiveTime(ctx, stored); err != nil {
		return stored, 0, err
	}
	return stored, rejected, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client hung up; nothing to do
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
