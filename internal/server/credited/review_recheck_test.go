// Regression tests from the cross-model re-check of ADR 0017's review fixes: deletion floor, migration probes, minute keys and device replacement.

package credited

import (
	"testing"
	"time"
)

func TestRecheckKeyVariants(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	k1 := Key{ComputedAt: m.Add(time.Minute), Snapshot: 1}
	k2 := Key{ComputedAt: m.Add(2 * time.Minute), Snapshot: 2}
	it := func(p string, n int, k Key) Item {
		return Item{Minute: m, Project: p, Label: "you", Credited: n, Key: k}
	}
	old := func(p string, n int, k Key, at time.Time) Stored {
		return Stored{Item: it(p, n, k), RulesVersion: "v1", UpdatedAt: at}
	}
	for _, tc := range []struct {
		name                     string
		now                      time.Time
		stored                   []Stored
		items                    []Item
		writes, removes, rejects int
	}{
		{"one project changes all rows rekey", m.Add(3 * time.Minute), []Stored{old("a", 20, k1, m), old("b", 20, k1, m)}, []Item{it("a", 10, k2), it("b", 20, k2)}, 2, 0, 0},
		{"legacy rows full replacement", m.Add(3 * time.Minute), []Stored{old("a", 20, Key{}, m), old("b", 20, Key{}, m)}, []Item{it("a", 10, k2), it("b", 20, k2)}, 2, 0, 0},
		{"legacy row blocks partial replacement", m.Add(3 * time.Minute), []Stored{old("a", 20, Key{}, m)}, []Item{it("b", 20, k2)}, 0, 0, 1},
		{"catchup same key", m.Add(time.Hour), []Stored{old("a", 20, k1, m.Add(59*time.Minute))}, []Item{it("b", 20, k1)}, 1, 0, 0},
		{"catchup different key", m.Add(time.Hour), []Stored{old("a", 20, k1, m.Add(59*time.Minute))}, []Item{it("b", 20, k2)}, 0, 0, 1},
		{"catchup first batch mixed", m.Add(time.Hour), nil, []Item{it("a", 20, k1), it("b", 20, k2)}, 0, 0, 2},
		{"catchup legacy refuses keyed addition", m.Add(time.Hour), []Stored{old("a", 20, Key{}, m.Add(59*time.Minute))}, []Item{it("b", 20, k2)}, 0, 0, 1},
		{"rewrite with new-key removal", m.Add(3 * time.Minute), []Stored{old("a", 20, k1, m), old("b", 20, k1, m)}, []Item{{Minute: m, Project: "a", Label: "idle", Key: k2}, it("b", 20, k2)}, 1, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in []Indexed
			for i, it := range tc.items {
				in = append(in, Indexed{Index: i, Item: it})
			}
			w, r, rej := Plan(in, tc.stored, Bounds{RulesVersion: "v1", Now: tc.now, Since: m.Add(-time.Hour)})
			if len(w) != tc.writes || len(r) != tc.removes || len(rej) != tc.rejects {
				t.Fatalf("w=%+v r=%+v rej=%+v", w, r, rej)
			}
		})
	}
}
