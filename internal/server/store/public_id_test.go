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
