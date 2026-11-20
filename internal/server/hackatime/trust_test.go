package hackatime

import (
	"context"
	"net/http"
	"sync/atomic"
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

// TestTrustRefreshWhenTokenRevoked covers a user who revokes Logbook's
// access on Hackatime. The public trust_factor endpoint is ambiguous (an alt
// account can take the user's numeric id as its username), so it must never
// be consulted: a ban sticks and anything else becomes unknown.
func TestTrustRefreshWhenTokenRevoked(t *testing.T) {
	tests := []struct {
		name      string
		stored    string // trust when linked
		remote    string // trust on Hackatime after the revoke
		wantLevel TrustLevel
	}{
		{"banned user stays banned", "red", "red", TrustRed},
		{"clean user becomes unverifiable", "green", "green", TrustUnknown},
		{"user banned after revoking is not seen as clean", "green", "red", TrustUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.newUser(t, "alice")
			e.fake.addUser(42, tt.stored)
			if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
				t.Fatalf("link = %d %s", rec.Code, rec.Body)
			}
			// An alt account named after alice's numeric Hackatime id.
			e.fake.addUser(777, "green")
			e.fake.setUsername(777, "42")
			e.fake.revokeAll(42)
			e.fake.setTrust(42, tt.remote)

			ctx := context.Background()
			r := NewTrustRefresher(e.st, e.box, e.client, e.logger, e.clock)
			if err := r.Refresh(ctx, alice); err != nil {
				t.Fatal(err)
			}
			if lvl, err := e.svc.TrustLevel(ctx, alice); err != nil || lvl != tt.wantLevel {
				t.Fatalf("TrustLevel() = %q, %v; want %q", lvl, err, tt.wantLevel)
			}
			if n := e.fake.trustFactorCalls(); n != 0 {
				t.Fatalf("public trust_factor endpoint called %d times", n)
			}
		})
	}
}

// TestTrustRefreshIgnoresSupersededLink: a level read for one link is not
// written onto the link that replaced it.
func TestTrustRefreshIgnoresSupersededLink(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	e.fake.addUser(43, "blue")
	e.fake.setTrust(42, "red")
	r := NewTrustRefresher(e.st, e.box, e.client, e.logger, e.clock)
	// Relink to account 43 while the refresh for account 42 is in flight.
	var fired atomic.Bool
	relinked := make(chan error, 1)
	e.fake.setMeHook(func(_ http.ResponseWriter, _ *http.Request) bool {
		if fired.CompareAndSwap(false, true) {
			relinked <- e.linkDirect(ctx, alice, 43)
		}
		return false
	})
	if err := r.Refresh(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := <-relinked; err != nil {
		t.Fatalf("relink: %v", err)
	}
	if lvl, _ := e.svc.TrustLevel(ctx, alice); lvl != TrustBlue {
		t.Fatalf("TrustLevel() = %q, want account 43's blue", lvl)
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
