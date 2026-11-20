package localdb

import (
	"context"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
)

func TestRepoMapJournal(t *testing.T) {
	ctx := context.Background()
	clock := testNow
	d, _ := openTest(t, WithClock(func() time.Time { return clock }))
	t1 := testNow.Add(-2 * time.Hour)
	record := func(at time.Time, repos ...struggle.Repo) {
		t.Helper()
		if err := d.RecordRepos(ctx, at, repos); err != nil {
			t.Fatal(err)
		}
	}
	record(t1, struggle.Repo{Path: "/src/app", Project: "old"}, struggle.Repo{Path: "", Project: "x"})
	record(t1.Add(time.Minute), struggle.Repo{Path: "/src/app", Project: "old"}) // unchanged: no row
	record(t1.Add(time.Hour), struggle.Repo{Path: "/src/app", Project: "new"}, struggle.Repo{Path: "/src/lib", Project: "lib"})
	rows, err := d.RepoMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoMapping{
		{Since: t1, Path: "/src/app", Project: "old"},
		{Since: t1.Add(time.Hour), Path: "/src/app", Project: "new"},
		{Since: t1.Add(time.Hour), Path: "/src/lib", Project: "lib"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i := range want {
		if !rows[i].Since.Equal(want[i].Since) || rows[i].Path != want[i].Path || rows[i].Project != want[i].Project {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	// "old" applies to minutes until the "new" row; it goes only once that
	// row is older than the retention cut.
	clock = t1.Add(time.Hour).Add(DefaultRetention)
	if _, err := d.PruneCrediting(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := d.RepoMap(ctx); len(rows) != 3 {
		t.Fatalf("pruned too early: %+v", rows)
	}
	clock = clock.Add(time.Minute)
	if _, err := d.PruneCrediting(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := d.RepoMap(ctx); len(rows) != 2 || rows[0].Project != "new" {
		t.Fatalf("after prune = %+v", rows)
	}
}
