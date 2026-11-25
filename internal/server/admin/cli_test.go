package admin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/admin"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

// The command line's flows: grant, key create (printed once, stored as its
// hash), list, key revoke, and revoke (role and every key); each audited.
func TestCommandLine(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	ctx := context.Background()
	keys := admin.NewKeys(st, func() time.Time { return testNow })
	ada := seedtest.User(t, st, "ada", "UTC", true)
	seedtest.User(t, st, "bob", "UTC", true)
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := admin.Command(ctx, keys, args, &out)
		return out.String(), err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}

	for _, args := range [][]string{nil, {"grant"}, {"key"}, {"key", "create", "ada"}, {"key", "list", "x"}, {"promote", "ada"}} {
		if _, err := run(args...); !errors.Is(err, admin.ErrUsage) {
			t.Fatalf("%v: %v, want usage", args, err)
		}
	}
	if _, err := run("grant", "nobody"); !errors.Is(err, admin.ErrUserNotFound) {
		t.Fatalf("grant nobody: %v", err)
	}
	if _, err := run("key", "create", "ada", "laptop"); !errors.Is(err, admin.ErrNotAdmin) {
		t.Fatalf("key for a non-admin: %v", err)
	}
	if out := mustRun("grant", "ada"); out != "ada is now an admin\n" {
		t.Fatalf("grant: %q", out)
	}
	if out := mustRun("grant", "ADA"); out != "ADA is already an admin\n" {
		t.Fatalf("grant again: %q", out)
	}
	if _, err := run("key", "create", "ada", " "); !errors.Is(err, admin.ErrInvalidName) {
		t.Fatalf("blank name: %v", err)
	}

	out := mustRun("key", "create", "ada", "fraud squad")
	key := regexp.MustCompile(`(?m)^lba_[A-Za-z0-9_-]{43}$`).FindString(out)
	if key == "" || !strings.Contains(out, "prefix "+key[:16]) {
		t.Fatalf("key create printed %q", out)
	}
	// Only the hash and the prefix are stored.
	var hash []byte
	var prefix string
	if err := st.Pool.QueryRow(ctx, `SELECT key_hash, prefix FROM admin_keys WHERE user_id = $1`, ada).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if !bytes.Equal(hash, sum[:]) || prefix != key[:16] {
		t.Fatalf("stored %x %q", hash, prefix)
	}
	var leaked int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE data::text LIKE '%' || $1 || '%'`, key[16:]).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the key is in %d audit rows (%v)", leaked, err)
	}
	a, err := keys.Authenticate(ctx, key)
	if err != nil || a.UserID != ada || a.Username != "ada" {
		t.Fatalf("authenticate = %+v, %v", a, err)
	}

	second := regexp.MustCompile(`(?m)^lba_\S+$`).FindString(mustRun("key", "create", "ada", "spare"))
	list := mustRun("key", "list")
	if !strings.Contains(list, key[:16]) || !strings.Contains(list, second[:16]) || !strings.Contains(list, "fraud squad") ||
		strings.Contains(list, key[16:]) || strings.Count(list, "live") != 2 {
		t.Fatalf("list:\n%s", list)
	}
	if out := mustRun("key", "revoke", second[:16]); out != "revoked "+second[:16]+"\n" {
		t.Fatalf("key revoke: %q", out)
	}
	if _, err := run("key", "revoke", second[:16]); !errors.Is(err, admin.ErrKeyNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
	if _, err := keys.Authenticate(ctx, second); !errors.Is(err, admin.ErrInvalidKey) {
		t.Fatalf("revoked key: %v", err)
	}
	if !strings.Contains(mustRun("key", "list"), "revoked 2026-07-01T12:00:00Z") {
		t.Fatal("list doesn't show the revocation")
	}

	if out := mustRun("revoke", "ada"); out != "ada is no longer an admin; 1 admin key(s) revoked\n" {
		t.Fatalf("revoke: %q", out)
	}
	if _, err := keys.Authenticate(ctx, key); !errors.Is(err, admin.ErrInvalidKey) {
		t.Fatalf("key after revoke: %v", err)
	}
	if _, err := run("key", "create", "ada", "again"); !errors.Is(err, admin.ErrNotAdmin) {
		t.Fatalf("key after revoke: %v", err)
	}

	var actions []string
	for _, action := range []string{"admin.grant", "admin_key.create", "admin_key.list", "admin_key.revoke", "admin.revoke"} {
		for _, r := range audit(t, st, action) {
			if r.Actor != nil || r.Data["via"] != "command line" {
				t.Fatalf("audit row %+v", r)
			}
			actions = append(actions, action)
		}
	}
	want := []string{"admin.grant", "admin.grant", "admin_key.create", "admin_key.create", "admin_key.list", "admin_key.list",
		"admin_key.revoke", "admin.revoke"}
	if strings.Join(actions, " ") != strings.Join(want, " ") {
		t.Fatalf("audited %v, want %v", actions, want)
	}
}
