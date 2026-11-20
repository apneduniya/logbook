package ingest_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/pkg/wire"
)

// now is the fixed clock every test uses.
var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return now }

// fakePublisher records MinutesChanged calls.
type fakePublisher struct {
	mu    sync.Mutex
	calls []published
}

type published struct {
	UserID   uuid.UUID
	From, To time.Time
}

func (p *fakePublisher) MinutesChanged(userID uuid.UUID, from, to time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, published{userID, from, to})
}

func (p *fakePublisher) Calls() []published {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]published(nil), p.calls...)
}

func newService(t testing.TB) (*ingest.Service, *store.Store, *fakePublisher) {
	t.Helper()
	st := storetest.New(t)
	pub := &fakePublisher{}
	return ingest.New(st, pub, ingest.WithClock(clock)), st, pub
}

// newUser inserts a user directly; creating accounts belongs to the auth module.
func newUser(t testing.TB, st *store.Store, timezone string) uuid.UUID {
	t.Helper()
	name := "u_" + uuid.NewString()[:8]
	var id uuid.UUID
	err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO users (hca_id, username, email, timezone) VALUES ($1::text, $1::text, $1::text || '@example.com', $2) RETURNING id`,
		name, timezone).Scan(&id)
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return id
}

type minuteRow struct {
	Minute                              time.Time
	Project, Language, Editor, Category string
	Label                               string
	Seconds                             int16
}

func minutesOf(t testing.TB, st *store.Store, userID uuid.UUID) []minuteRow {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(),
		`SELECT minute, project, language, editor, category, label, seconds
		 FROM user_minutes WHERE user_id = $1
		 ORDER BY minute, project, language, editor, category, label`, userID)
	if err != nil {
		t.Fatalf("reading user_minutes: %v", err)
	}
	defer rows.Close()
	var out []minuteRow
	for rows.Next() {
		var r minuteRow
		if err := rows.Scan(&r.Minute, &r.Project, &r.Language, &r.Editor, &r.Category, &r.Label, &r.Seconds); err != nil {
			t.Fatalf("scanning user_minutes: %v", err)
		}
		r.Minute = r.Minute.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading user_minutes: %v", err)
	}
	return out
}

func countRows(t testing.TB, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

// unix converts t to WakaTime's float seconds.
func unix(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

func beat(at time.Time, project string) wire.Heartbeat {
	return wire.Heartbeat{
		Entity: "/home/me/" + project + "/main.go", Type: "file", Category: "coding",
		Time: unix(at), Project: project, Language: "Go", Editor: "vscode",
	}
}

// randomTimeline returns n heartbeats on increasing, distinct times starting
// at start, with mostly short gaps and occasional breaks.
func randomTimeline(rng *rand.Rand, start time.Time, n int) []wire.Heartbeat {
	projects := []string{"alpha", "beta", ""}
	languages := []string{"Go", "Rust", "Python"}
	out := make([]wire.Heartbeat, n)
	cur := start
	for i := range out {
		gap := time.Duration(1+rng.IntN(90_000)) * time.Millisecond
		if rng.IntN(12) == 0 {
			gap = time.Duration(1+rng.IntN(600)) * time.Second
		}
		cur = cur.Add(gap)
		h := beat(cur, projects[rng.IntN(len(projects))])
		h.Language = languages[rng.IntN(len(languages))]
		h.Entity = fmt.Sprintf("/src/file%d.go", i)
		out[i] = h
	}
	return out
}
