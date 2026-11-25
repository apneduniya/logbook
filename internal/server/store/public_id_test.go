package store_test

import (
	"context"
	"testing"

	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

// TestPublicIDsAndReservedUsername pins migration 00007: every user gets a
// distinct numeric public_id above Hackatime's id range, and nobody can hold
// the username "my" in any case.
func TestPublicIDsAndReservedUsername(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	a := seedtest.User(t, st, "alice", "UTC", true)
	b := seedtest.User(t, st, "bob", "UTC", true)
	pa, err := st.SeedTestPublicID(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := st.SeedTestPublicID(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if pa == pb || pa < 100000001 || pb < 100000001 {
		t.Fatalf("public ids = %d, %d; want distinct and >= 100000001", pa, pb)
	}
	for _, name := range []string{"my", "MY", "My"} {
		_, err := st.SeedTestUser(ctx, db.SeedTestUserParams{
			HcaID: "hca-" + name, Username: name, Email: "x@example.com", Timezone: "UTC",
		})
		if err == nil {
			t.Fatalf("username %q was accepted", name)
		}
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE users SET public_id = 1 WHERE id = $1", a); err == nil {
		t.Fatal("public_id was writable")
	}
}

// TestReservedUsernameRenameAvoidsTakenName: migration 00007 renames a user
// called "my" to my-user-<public_id>. When someone already holds that name
// it picks the next free my-user-<public_id>-<n> instead of failing on the
// unique username, and audits the name it actually chose.
func TestReservedUsernameRenameAvoidsTakenName(t *testing.T) {
	p := storetest.NewPartial(t, 6)
	ctx := context.Background()
	// "my" is inserted first, so it is numbered 100000001; the two names it
	// would try first are taken (usernames are case-insensitive).
	for _, name := range []string{"my", "my-user-100000001", "My-User-100000001-2"} {
		if _, err := p.Store.Pool.Exec(ctx,
			`INSERT INTO users (hca_id, username, email) VALUES ($1, $2, 'x@example.com')`, "hca-"+name, name); err != nil {
			t.Fatal(err)
		}
	}
	p.UpTo(t, 7)

	var publicID int64
	var got string
	if err := p.Store.Pool.QueryRow(ctx, `SELECT public_id, username::text FROM users WHERE hca_id = 'hca-my'`).
		Scan(&publicID, &got); err != nil {
		t.Fatal(err)
	}
	if publicID != 100000001 {
		t.Fatalf("setup: \"my\" got public id %d, want 100000001", publicID)
	}
	if want := "my-user-100000001-3"; got != want {
		t.Fatalf("renamed to %q, want %q", got, want)
	}
	var from, to string
	if err := p.Store.Pool.QueryRow(ctx, `SELECT data->>'from', data->>'to' FROM audit_log
		WHERE action = 'user.username_reserved'`).Scan(&from, &to); err != nil {
		t.Fatal(err)
	}
	if from != "my" || to != got {
		t.Fatalf("audit from %q to %q, want my to %q", from, to, got)
	}
}
