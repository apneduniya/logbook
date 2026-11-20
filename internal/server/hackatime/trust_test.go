package hackatime

import (
	"context"
	"testing"
	"time"
)

func TestTrustRefresh(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	// trust_checked_at is written by the database clock, so use real time here.
	r := NewTrustRefresher(e.st, e.box, e.client, e.logger, nil)

	// Fresh links aren't due.
	if due, err := r.DueUsers(ctx); err != nil || len(due) != 0 {
		t.Fatalf("DueUsers() = %v, %v; want none", due, err)
	}
	if _, err := e.st.Pool.Exec(ctx, "UPDATE hackatime_links SET trust_checked_at = $1", time.Now().Add(-TrustMaxAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if due, err := r.DueUsers(ctx); err != nil || len(due) != 1 || due[0] != alice {
		t.Fatalf("DueUsers() = %v, %v; want alice", due, err)
	}

	e.fake.setTrust(42, "red")
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	lvl, err := e.svc.TrustLevel(ctx, alice)
	if err != nil || lvl != TrustRed || !lvl.Banned() {
		t.Fatalf("TrustLevel() = %q, %v; want red", lvl, err)
	}
	if n := e.count(t, "SELECT count(*) FROM audit_log WHERE action = 'hackatime.trust_changed' AND subject_id = $1", alice.String()); n != 1 {
		t.Fatalf("trust change audit rows = %d", n)
	}

	// Unchanged levels don't add audit rows.
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, "SELECT count(*) FROM audit_log WHERE action = 'hackatime.trust_changed'"); n != 1 {
		t.Fatalf("trust change audit rows = %d, want 1", n)
	}
}

func TestTrustRefreshFallsBackWhenTokenRevoked(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	r := NewTrustRefresher(e.st, e.box, e.client, e.logger, e.clock)

	// The user revokes Logbook on Hackatime, then gets banned.
	e.fake.revokeAll(42)
	e.fake.setTrust(42, "red")
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if lvl, _ := e.svc.TrustLevel(ctx, alice); lvl != TrustRed {
		t.Fatalf("TrustLevel() = %q, want red via public endpoint", lvl)
	}
}

func TestTrustRefreshUnlinkedIsNoop(t *testing.T) {
	e := newEnv(t)
	bob := e.newUser(t, "bob")
	r := NewTrustRefresher(e.st, e.box, e.client, e.logger, e.clock)
	if err := r.Refresh(context.Background(), bob); err != nil {
		t.Fatalf("Refresh(unlinked) err = %v", err)
	}
}
