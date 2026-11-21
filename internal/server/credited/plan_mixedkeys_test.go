package credited

import (
	"testing"
	"time"
)

// A device-minute is one computation: a batch that would leave rows of two
// keys in one minute changes nothing in it (ADR 0017), while replacing the
// whole minute under a new key is accepted.
func TestPlanRejectsMixedKeysInOpenMinute(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	k1 := Key{ComputedAt: m.Add(time.Minute), Snapshot: 1}
	k2 := Key{ComputedAt: m.Add(2 * time.Minute), Snapshot: 2}
	item := func(p string, secs int, k Key) Item {
		return Item{Minute: m, Project: p, Label: "you", Credited: secs, Key: k}
	}
	removal := func(p string, k Key) Item { return Item{Minute: m, Project: p, Label: "idle", Key: k} }
	b := Bounds{RulesVersion: "v1", Now: m.Add(3 * time.Minute), Since: m.Add(-time.Hour)}
	storedA := []Stored{{Item: item("a", 30, k1), RulesVersion: "v1", UpdatedAt: m.Add(time.Minute)}}

	for _, tc := range []struct {
		name       string
		items      []Item
		stored     []Stored
		wantWrites int
		wantMixed  int
	}{
		{"two keys in one request", []Item{item("a", 30, k1), item("b", 30, k2)}, nil, 0, 2},
		{"new key beside an untouched row of an older key", []Item{item("b", 30, k2)}, storedA, 0, 1},
		{"whole minute replaced under a new key", []Item{item("a", 20, k2), item("b", 30, k2)}, storedA, 2, 0},
		{"project replaced, removal carries the new key", []Item{removal("a", k2), item("b", 30, k2)}, storedA, 1, 0},
		{"same key adds a project", []Item{item("b", 30, k1)}, storedA, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in []Indexed
			for i, it := range tc.items {
				in = append(in, Indexed{Index: i, Item: it})
			}
			writes, _, rejected := Plan(in, tc.stored, b)
			mixed := 0
			for _, r := range rejected {
				if r.Error == errMixedKeys.Error() {
					mixed++
				}
			}
			for _, r := range rejected {
				// A mixed-key refusal carries the minute's rows, so the
				// device adopts them instead of dropping its view.
				if r.Error == errMixedKeys.Error() && r.Stored == nil {
					t.Fatalf("mixed-key refusal without stored rows: %+v", r)
				}
			}
			if len(writes) != tc.wantWrites || mixed != tc.wantMixed {
				t.Fatalf("writes = %+v, rejected = %+v; want %d writes, %d mixed-key rejections", writes, rejected, tc.wantWrites, tc.wantMixed)
			}
		})
	}
}

// A migration probe (a keyed removal) next to a legacy row the agent
// doesn't know of is refused with the minute's rows, which the agent then
// adopts, instead of being refused without them and dropped.
func TestPlanMixedKeysAgainstLegacyRowsReturnsStored(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	legacy := func(p string) Stored {
		return Stored{Item: Item{Minute: m, Project: p, Label: "you", Credited: 30}, RulesVersion: "v1", UpdatedAt: m.Add(time.Minute)}
	}
	probe := Item{Minute: m, Project: "a", Label: "idle", Key: Key{ComputedAt: m.Add(time.Minute)}}
	_, removes, rejected := Plan([]Indexed{{Index: 0, Item: probe}}, []Stored{legacy("a"), legacy("b")},
		Bounds{RulesVersion: "v1", Now: m.Add(5 * time.Minute), Since: m.Add(-time.Hour)})
	if len(removes) != 0 || len(rejected) != 1 || len(rejected[0].Stored) != 2 {
		t.Fatalf("removes = %+v, rejected = %+v; want the probe refused with both stored rows", removes, rejected)
	}
}
