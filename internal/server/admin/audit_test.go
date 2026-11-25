package admin_test

import (
	"context"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/admin"
)

// Every request with a live key writes admin.read rows naming the admin,
// the path, the key's prefix (never the key) and each user it read; a
// request that fails or reads no user is audited against the key.
func TestEveryRequestIsAudited(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := strconv.FormatInt(f.orpheusID, 10)
	pat := strconv.FormatInt(f.patID, 10)
	prefix := f.key[:16]
	keyID := keyIDOf(t, f, prefix)

	cases := []struct {
		c        call
		status   int
		subjects []uuid.UUID // nil: the key itself
	}{
		{call{target: "/api/admin/v1/user/info?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/info_batch?ids=" + id + "," + pat}, http.StatusOK, []uuid.UUID{f.orpheus, f.pat}},
		{call{target: "/api/admin/v1/user/heartbeats?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/projects?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/trust_logs?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{method: http.MethodPost, target: "/api/admin/v1/user/get_user_by_email", body: `{"email": "pat@example.com"}`}, http.StatusOK, []uuid.UUID{f.pat}},
		{call{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", body: `{"query": "orph"}`}, http.StatusOK, []uuid.UUID{f.orpheus, f.otto}},
		{call{target: "/api/admin/v1/user/devices?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/minutes?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/submissions?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/audit_log?id=" + id}, http.StatusOK, []uuid.UUID{f.orpheus}},
		{call{target: "/api/admin/v1/user/info?id=12345"}, http.StatusNotFound, nil},
		{call{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", body: `{"query": "zzz"}`}, http.StatusOK, nil},
		{call{target: "/api/admin/v1/submissions/" + uuid.NewString()}, http.StatusNotFound, nil},
		{call{target: "/api/admin/v1/user/stats?id=" + id}, http.StatusNotFound, nil},
	}
	for _, tc := range cases {
		before := len(audit(t, f.st, "admin.read"))
		f.expect(t, tc.c, tc.status)
		rows := audit(t, f.st, "admin.read")[before:]
		path, _, _ := strings.Cut(tc.c.target, "?")
		var got []uuid.UUID
		for _, r := range rows {
			if r.Actor == nil || *r.Actor != f.fiona || r.Data["path"] != path || r.Data["key"] != prefix {
				t.Fatalf("%s: audit row %+v", tc.c.target, r)
			}
			if strings.Contains(r.SubjectID, f.key) || strings.Contains(r.Data["key"].(string), f.key[16:]) {
				t.Fatalf("%s: the key itself is in the audit log", tc.c.target)
			}
			switch r.SubjectType {
			case "user":
				got = append(got, uuid.MustParse(r.SubjectID))
			case "admin_key":
				if r.SubjectID != keyID.String() || len(rows) != 1 {
					t.Fatalf("%s: key audit row %+v", tc.c.target, r)
				}
			default:
				t.Fatalf("%s: subject type %q", tc.c.target, r.SubjectType)
			}
		}
		if len(rows) == 0 {
			t.Fatalf("%s: not audited", tc.c.target)
		}
		slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		want := slices.Clone(tc.subjects)
		slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		if !slices.Equal(got, want) {
			t.Fatalf("%s: audited subjects %v, want %v", tc.c.target, got, want)
		}
	}
	// The parameters say what was searched; a failure's row has its status.
	rows := audit(t, f.st, "admin.read")
	var failed bool
	for _, r := range rows {
		if r.Data["path"] == "/api/admin/v1/user/stats" && r.Data["status"] == float64(http.StatusNotFound) {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the failed request's audit row has no status")
	}
	var searched bool
	for _, r := range rows {
		if p, _ := r.Data["params"].(map[string]any); p["email"] == "pat@example.com" {
			searched = true
		}
	}
	if !searched {
		t.Fatal("the email searched for isn't in the audit row")
	}
}

func keyIDOf(t *testing.T, f *fixture, prefix string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.st.Pool.QueryRow(context.Background(), `SELECT id FROM admin_keys WHERE prefix = $1`, prefix).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The admin API changes no user data: every query it serves from is a
// SELECT, and calling every endpoint leaves every table but the audit log
// and the keys' last use exactly as it was.
func TestReadOnly(t *testing.T) {
	t.Parallel()

	t.Run("its queries are SELECTs", func(t *testing.T) {
		raw, err := os.ReadFile("../store/queries/admin.sql")
		if err != nil {
			t.Fatal(err)
		}
		names := regexp.MustCompile(`(?m)^-- name: (\w+) `).FindAllStringSubmatch(string(raw), -1)
		writes := regexp.MustCompile(`(?i)\b(insert|update|delete|truncate|merge|alter|drop|create|grant|copy|call|lock)\b|\bfor\s+(no\s+key\s+)?update\b|\bnextval\b|\bsetval\b|pg_advisory`)
		for _, stmt := range strings.Split(string(raw), "-- name: ")[1:] {
			var body []string
			for _, line := range strings.Split(stmt, "\n")[1:] {
				if !strings.HasPrefix(strings.TrimSpace(line), "--") {
					body = append(body, line)
				}
			}
			sql := strings.TrimSpace(strings.Join(body, "\n"))
			if m := writes.FindString(sql); m != "" {
				t.Fatalf("%s has %q:\n%s", strings.Fields(stmt)[0], m, sql)
			}
			if !strings.HasPrefix(sql, "SELECT") && !strings.HasPrefix(sql, "WITH") {
				t.Fatalf("%s isn't a SELECT", strings.Fields(stmt)[0])
			}
		}
		// Reads uses exactly the queries of admin.sql.
		var want []string
		for _, n := range names {
			want = append(want, n[1])
		}
		rt := reflect.TypeFor[admin.Reads]()
		var got []string
		for i := range rt.NumMethod() {
			got = append(got, rt.Method(i).Name)
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("Reads = %v\nadmin.sql = %v", got, want)
		}
		// Evidence, the one other way to data, only reads.
		et := reflect.TypeFor[admin.Evidence]()
		var ev []string
		for i := range et.NumMethod() {
			ev = append(ev, et.Method(i).Name)
		}
		if !slices.Equal(ev, []string{"CheckBundle", "PartRows", "WriteBundle"}) {
			t.Fatalf("Evidence = %v", ev)
		}
	})

	t.Run("calls change no user data", func(t *testing.T) {
		f := newFixture(t)
		before := snapshot(t, f)
		id := strconv.FormatInt(f.orpheusID, 10)
		for _, c := range []call{
			{target: "/api/admin/v1/user/info?id=" + id},
			{target: "/api/admin/v1/user/info_batch?ids=" + id},
			{target: "/api/admin/v1/user/heartbeats?include_ai=true&id=" + id},
			{target: "/api/admin/v1/user/projects?start_date=2026-01-01&id=" + id},
			{target: "/api/admin/v1/user/trust_logs?id=" + id},
			{method: http.MethodPost, target: "/api/admin/v1/user/get_user_by_email", body: `{"email": "orpheus@example.com"}`},
			{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", body: `{"query": "o"}`},
			{target: "/api/admin/v1/user/devices?id=" + id},
			{target: "/api/admin/v1/user/minutes?id=" + id},
			{target: "/api/admin/v1/user/submissions?id=" + id},
			{target: "/api/admin/v1/user/audit_log?id=" + id},
			{target: "/api/admin/v1/submissions/" + uuid.NewString()},
			{target: "/api/admin/v1/submissions/" + uuid.NewString() + "/evidence"},
			{target: "/api/admin/v1/submissions/" + uuid.NewString() + "/bundle"},
			{method: http.MethodPost, target: "/api/admin/v1/user/convict", body: `{"id": ` + id + `, "trust_level": "red"}`},
			{method: http.MethodDelete, target: "/api/admin/v1/user/info?id=" + id},
		} {
			_ = f.do(t, c)
		}
		if after := snapshot(t, f); !reflect.DeepEqual(before, after) {
			for table := range before {
				if before[table] != after[table] {
					t.Errorf("table %s changed", table)
				}
			}
		}
	})
}

// snapshot hashes the contents of every table except the audit log, the
// admin keys (their last use moves) and River's.
func snapshot(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := f.st.Pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public'
		AND tablename NOT IN ('audit_log', 'admin_keys', 'goose_db_version') AND tablename NOT LIKE 'river%'
		AND tablename NOT LIKE 'heartbeats\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := collect(rows)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, table := range tables {
		var sum string
		q := `SELECT coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM "` + table + `" t`
		if err := f.st.Pool.QueryRow(ctx, q).Scan(&sum); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		out[table] = sum
	}
	if len(out) < 10 {
		t.Fatalf("only %d tables: %v", len(out), tables)
	}
	return out
}

type stringRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

func collect(rows stringRows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
