package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Spool drain limits.
const (
	spoolBatch     = 500
	maxSpoolLine   = 1 << 20
	spoolOffsetKey = "spool.offset" // "<draining file name>:<byte offset>"
	drainingSuffix = ".draining"
	// A claim blocked by another program retries after 10, 20, 40 and 80 ms.
	claimAttempts = 5
	claimBackoff  = 10 * time.Millisecond
)

// EventStore is where drained events go. *localdb.DB satisfies it.
type EventStore interface {
	InsertEvents(ctx context.Context, evs []wire.LocalEvent, marks ...localdb.KV) error
	Get(ctx context.Context, key string) (string, error)
}

// Spool drains the hook spool file (config.SpoolPath) into the database.
//
// Hook commands append JSON lines to the spool when the daemon is down. To
// read it without losing concurrent appends, the drainer first renames the
// file to a unique "<spool>.<n>.draining" name, so new appends create a fresh
// spool, then waits a grace period for any hook that opened the old file just
// before the rename to finish its write. Progress is stored with each batch,
// so a crash mid-drain neither loses nor duplicates events.
type Spool struct {
	path  string
	store EventStore
	log   *slog.Logger
	grace time.Duration
	now   func() time.Time
}

// NewSpool returns a drainer for the spool at path.
func NewSpool(path string, store EventStore, log *slog.Logger, grace time.Duration) *Spool {
	return &Spool{path: path, store: store, log: log, grace: grace, now: time.Now}
}

// DrainResult counts what a drain did.
type DrainResult struct{ Stored, Invalid int }

// Drain stores every event in the spool, finishing any drain a previous
// run left behind first.
func (s *Spool) Drain(ctx context.Context) (DrainResult, error) {
	var total DrainResult
	leftovers, err := s.leftovers()
	if err != nil {
		return total, err
	}
	fresh, err := s.claim(ctx)
	if err != nil {
		return total, err
	}
	if fresh != "" {
		if err := sleep(ctx, s.grace); err != nil {
			return total, err
		}
		leftovers = append(leftovers, fresh)
	}
	for _, f := range leftovers {
		r, err := s.drainFile(ctx, f)
		total.Stored += r.Stored
		total.Invalid += r.Invalid
		if err != nil {
			return total, err
		}
	}
	if total.Invalid > 0 {
		s.log.WarnContext(ctx, "skipped invalid spooled events", "count", total.Invalid)
	}
	return total, nil
}

// leftovers lists draining files an earlier drain didn't finish, oldest
// first. It matches names by prefix rather than glob, since the data dir
// may contain glob metacharacters.
func (s *Spool) leftovers() ([]string, error) {
	dir, prefix := filepath.Dir(s.path), filepath.Base(s.path)+"."
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing spool dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, drainingSuffix) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Strings(out) // names embed a zero-padded timestamp
	return out, nil
}

// claim renames the live spool to a unique draining name. It returns "" if
// there is no spool.
//
// Hooks open the spool with delete sharing (package spoolfile), so on
// Windows the rename works while they append. Another program can still hold
// it without that sharing for a moment (an antivirus scan, a backup tool), so
// a sharing violation is retried briefly; if it persists, the next drain
// claims the file.
func (s *Spool) claim(ctx context.Context) (string, error) {
	name := fmt.Sprintf("%s.%020d%s", s.path, s.now().UnixNano(), drainingSuffix)
	backoff := claimBackoff
	for attempt := 1; ; attempt++ {
		err := os.Rename(s.path, name)
		switch {
		case err == nil:
			return name, nil
		case errors.Is(err, fs.ErrNotExist):
			return "", nil
		case attempt < claimAttempts && isSharingViolation(err):
			if err := sleep(ctx, backoff); err != nil {
				return "", err
			}
			backoff *= 2
		default:
			return "", fmt.Errorf("claiming spool: %w", err)
		}
	}
}

func (s *Spool) drainFile(ctx context.Context, path string) (DrainResult, error) {
	var res DrainResult
	f, err := os.Open(path) //nolint:gosec // path is our own spool file in the data dir
	if err != nil {
		return res, fmt.Errorf("opening spool: %w", err)
	}
	defer func() { _ = f.Close() }()

	base := filepath.Base(path)
	offset, err := s.resumeOffset(ctx, base)
	if err != nil {
		return res, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return res, fmt.Errorf("seeking spool: %w", err)
	}
	r := bufio.NewReaderSize(f, 64<<10)
	batch := make([]wire.LocalEvent, 0, spoolBatch)
	flush := func() error {
		mark := localdb.KV{Key: spoolOffsetKey, Value: base + ":" + strconv.FormatInt(offset, 10)}
		if err := s.store.InsertEvents(ctx, batch, mark); err != nil {
			return fmt.Errorf("storing spooled events: %w", err)
		}
		res.Stored += len(batch)
		batch = batch[:0]
		return nil
	}
	for {
		line, n, tooLong, err := readLine(r)
		offset += n
		if tooLong {
			res.Invalid++
		} else if len(line) > 0 {
			var ev wire.LocalEvent
			if json.Unmarshal(line, &ev) != nil || ev.Validate() != nil {
				res.Invalid++
			} else {
				batch = append(batch, ev)
			}
		}
		if len(batch) == spoolBatch {
			if ferr := flush(); ferr != nil {
				return res, ferr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("reading spool: %w", err)
		}
	}
	// The final flush also records the end offset, even for an empty batch,
	// so a crash before the remove below doesn't re-read the file.
	if err := flush(); err != nil {
		return res, err
	}
	_ = f.Close() // Windows can't remove an open file
	if err := os.Remove(path); err != nil {
		return res, fmt.Errorf("removing drained spool: %w", err)
	}
	return res, nil
}

// resumeOffset returns where a previous drain of the named file stopped.
func (s *Spool) resumeOffset(ctx context.Context, base string) (int64, error) {
	v, err := s.store.Get(ctx, spoolOffsetKey)
	if err != nil {
		return 0, err
	}
	name, off, ok := strings.Cut(v, ":")
	if !ok || name != base {
		return 0, nil
	}
	n, err := strconv.ParseInt(off, 10, 64)
	if err != nil || n < 0 {
		return 0, nil //nolint:nilerr // a corrupt mark means start from the top
	}
	return n, nil
}

// readLine reads one newline-terminated line and returns it trimmed, plus
// the bytes consumed. A line longer than maxSpoolLine is consumed but not
// kept (tooLong is set), so a corrupt spool can't exhaust memory.
func readLine(r *bufio.Reader) (line []byte, consumed int64, tooLong bool, err error) {
	var buf []byte
	for {
		var chunk []byte
		chunk, err = r.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !tooLong {
			if len(buf)+len(chunk) > maxSpoolLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSpace(buf), consumed, tooLong, err
	}
}

// sleep waits d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
