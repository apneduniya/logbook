package hackatime

import (
	"context"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// TestTrustHistory: linking and every refresh record the level read; a
// repeated level extends the newest row, a change starts a new one.
func TestTrustHistory(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	bob := e.linkedUser(t, "bob", 43)
	ctx := context.Background()
	r := NewTrustRefresher(e.st, e.box, e.client, e.logger, e.clock)

	history := func() []db.ListHackatimeTrustHistoryRow {
		t.Helper()
		rows, err := e.st.ListHackatimeTrustHistory(ctx, db.ListHackatimeTrustHistoryParams{UserID: alice, Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	first := history()
	if len(first) != 1 {
		t.Fatalf("after linking: %+v, want one row", first)
	}
	linkedLevel := first[0].Level

	e.advance(time.Hour)
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	h := history()
	if len(h) != 1 || h[0].Level != linkedLevel || !h[0].CheckedAt.Equal(e.clock()) {
		t.Fatalf("same level: %+v, want the one row moved to %v", h, e.clock())
	}

	e.advance(time.Hour)
	e.fake.setTrust(42, "red")
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Hour)
	e.fake.setTrust(42, "green")
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	h = history()
	want := []string{"green", "red", linkedLevel} // newest first
	if len(h) != len(want) {
		t.Fatalf("history = %+v, want %v", h, want)
	}
	for i, lvl := range want {
		if h[i].Level != lvl {
			t.Fatalf("history[%d] = %q, want %q (%+v)", i, h[i].Level, lvl, h)
		}
	}

	// Another user's history is separate.
	rows, err := e.st.ListHackatimeTrustHistory(ctx, db.ListHackatimeTrustHistoryParams{UserID: bob, Limit: 25})
	if err != nil || len(rows) != 1 {
		t.Fatalf("bob's history = %+v, %v; want his one linking row", rows, err)
	}
}
