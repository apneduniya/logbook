package credited

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)

func at(h, m int) time.Time { return time.Date(2026, 11, 20, h, m, 0, 0, time.UTC) }

func TestParse(t *testing.T) {
	ok := RawItem{Minute: "2026-11-20T11:59:00Z", Project: "logbook", Label: "you", Credited: 45}
	with := func(f func(*RawItem)) RawItem { r := ok; f(&r); return r }
	tests := []struct {
		name    string
		raw     RawItem
		wantErr error
	}{
		{"valid", ok, nil},
		{"no project", with(func(r *RawItem) { r.Project = ""; r.Label = "idle"; r.Credited = 0 }), nil},
		{"agent minute", with(func(r *RawItem) { r.Label = "agent"; r.Credited = 0; r.Cuts = []string{"agent_alone"} }), nil},
		{"two minutes ahead", with(func(r *RawItem) { r.Minute = "2026-11-20T12:02:00Z" }), nil},
		{"three minutes ahead", with(func(r *RawItem) { r.Minute = "2026-11-20T12:03:00Z" }), errMinuteFuture},
		{"offline catch-up a week later", with(func(r *RawItem) { r.Minute = "2026-11-13T09:00:00Z" }), nil},
		{"before 2024", with(func(r *RawItem) { r.Minute = "2023-12-31T23:59:00Z" }), errMinuteTooOld},
		{"seconds", with(func(r *RawItem) { r.Minute = "2026-11-20T11:59:30Z" }), errMinuteFormat},
		{"fraction", with(func(r *RawItem) { r.Minute = "2026-11-20T11:59:00.5Z" }), errMinuteFormat},
		{"offset", with(func(r *RawItem) { r.Minute = "2026-11-20T17:29:00+05:30" }), errMinuteFormat},
		{"no zone", with(func(r *RawItem) { r.Minute = "2026-11-20T11:59:00" }), errMinuteFormat},
		{"unix number as string", with(func(r *RawItem) { r.Minute = "1763639940" }), errMinuteFormat},
		{"long project", with(func(r *RawItem) { r.Project = strings.Repeat("a", 257) }), errProject},
		{"256-byte project", with(func(r *RawItem) { r.Project = strings.Repeat("a", 256) }), nil},
		{"control char in project", with(func(r *RawItem) { r.Project = "a\nb" }), errProject},
		{"invalid utf-8", with(func(r *RawItem) { r.Project = "a\xffb" }), errProject},
		{"bad label", with(func(r *RawItem) { r.Label = "me" }), errLabel},
		{"negative credit", with(func(r *RawItem) { r.Credited = -1 }), errCredited},
		{"61 seconds", with(func(r *RawItem) { r.Credited = 61 }), errCredited},
		{"credit on agent minute", with(func(r *RawItem) { r.Label = "agent"; r.Credited = 10 }), errCredited},
		{"bad cut code", with(func(r *RawItem) { r.Cuts = []string{"Rubber Stamp"} }), errCodes},
		{"too many flags", with(func(r *RawItem) { r.Flags = make([]string, MaxCodes+1) }), errCodes},
		{"server-only flag", with(func(r *RawItem) { r.Flags = []string{"multi_device"} }), errServerFlag},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			valid, rejected := Parse([]RawItem{tc.raw}, now)
			if tc.wantErr == nil {
				if len(rejected) != 0 || len(valid) != 1 {
					t.Fatalf("rejected = %+v", rejected)
				}
				return
			}
			if len(rejected) != 1 || rejected[0].Error != tc.wantErr.Error() {
				t.Fatalf("rejected = %+v, want %q", rejected, tc.wantErr)
			}
		})
	}
}

func TestParseNormalizesCodes(t *testing.T) {
	valid, _ := Parse([]RawItem{{
		Minute: "2026-11-20T11:59:00Z", Label: "you", Credited: 0,
		Cuts: []string{"rubber_stamp", "loop", "rubber_stamp"}, Flags: nil,
	}}, now)
	it := valid[0].Item
	if !slices.Equal(it.Cuts, []string{"loop", "rubber_stamp"}) || it.Flags == nil || len(it.Flags) != 0 {
		t.Fatalf("cuts %q flags %#v", it.Cuts, it.Flags)
	}
}

func you(minute time.Time, project string, credited int) Item {
	return Item{Minute: minute, Project: project, Label: "you", Credited: credited, Cuts: []string{}, Flags: []string{}}
}

func stored(it Item) Stored { return Stored{Item: it, RulesVersion: "v1"} }

func indexed(items ...Item) []Indexed {
	out := make([]Indexed, len(items))
	for i, it := range items {
		out[i] = Indexed{Index: i, Item: it}
	}
	return out
}

func TestPlan(t *testing.T) {
	fresh := at(11, 50)               // 10 minutes old: rewritable
	final := at(11, 30)               // exactly 30 minutes old: final
	old := at(9, 0)                   // long final
	agent := you(fresh, "logbook", 0) // shape reused below
	agent.Label, agent.Cuts = "agent", []string{"agent_alone"}

	tests := []struct {
		name      string
		stored    []Stored
		items     []Item
		wantWrite []string // "hh:mm project credited"
		wantRej   map[int]error
	}{
		{
			name:      "new minutes",
			items:     []Item{you(fresh, "logbook", 60), you(old, "logbook", 30)},
			wantWrite: []string{"09:00 logbook 30", "11:50 logbook 60"},
		},
		{
			name:   "exact repeat of a final minute is accepted without a write",
			stored: []Stored{stored(you(old, "logbook", 30))},
			items:  []Item{you(old, "logbook", 30)},
		},
		{
			name:    "changing a final minute is rejected",
			stored:  []Stored{stored(you(final, "logbook", 30))},
			items:   []Item{you(final, "logbook", 40)},
			wantRej: map[int]error{0: errFinal},
		},
		{
			name:      "changing a fresh minute is allowed",
			stored:    []Stored{stored(you(fresh, "logbook", 30))},
			items:     []Item{agent},
			wantWrite: []string{"11:50 logbook 0"},
		},
		{
			name:      "first write of an old minute is allowed",
			stored:    []Stored{stored(you(old, "logbook", 30))},
			items:     []Item{you(old, "site", 30)},
			wantWrite: []string{"09:00 site 30"},
		},
		{
			name:      "split within the cap",
			items:     []Item{you(fresh, "a", 30), you(fresh, "b", 30)},
			wantWrite: []string{"11:50 a 30", "11:50 b 30"},
		},
		{
			name:      "batch over the cap rejects the item that exceeds it",
			items:     []Item{you(fresh, "a", 40), you(fresh, "b", 30), you(fresh, "c", 20)},
			wantWrite: []string{"11:50 a 40", "11:50 c 20"},
			wantRej:   map[int]error{1: errOverMinuteCap},
		},
		{
			name:    "stored rows of other projects count towards the cap",
			stored:  []Stored{stored(you(old, "a", 50))},
			items:   []Item{you(old, "b", 20)},
			wantRej: map[int]error{0: errOverMinuteCap},
		},
		{
			name:      "credit moves between projects in one batch, raise listed first",
			stored:    []Stored{stored(you(fresh, "a", 60))},
			items:     []Item{you(fresh, "b", 60), you(fresh, "a", 0)},
			wantWrite: []string{"11:50 a 0", "11:50 b 60"},
		},
		{
			name:      "duplicate in batch keeps the first",
			items:     []Item{you(fresh, "a", 10), you(fresh, "a", 20)},
			wantWrite: []string{"11:50 a 10"},
			wantRej:   map[int]error{1: errDuplicate},
		},
		{
			name:      "a different rules version is a change",
			stored:    []Stored{{Item: you(fresh, "a", 10), RulesVersion: "v0"}},
			items:     []Item{you(fresh, "a", 10)},
			wantWrite: []string{"11:50 a 10"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			write, rejected := Plan(indexed(tc.items...), tc.stored, "v1", now)
			got := make([]string, len(write))
			for i, w := range write {
				got[i] = fmt.Sprintf("%s %s %d", w.Minute.Format("15:04"), w.Project, w.Credited)
			}
			if !slices.Equal(got, tc.wantWrite) && (len(got) != 0 || len(tc.wantWrite) != 0) {
				t.Errorf("write = %q, want %q", got, tc.wantWrite)
			}
			if len(rejected) != len(tc.wantRej) {
				t.Fatalf("rejected = %+v, want %v", rejected, tc.wantRej)
			}
			for _, r := range rejected {
				if want, ok := tc.wantRej[r.Index]; !ok || r.Error != want.Error() {
					t.Errorf("rejected[%d] = %q, want %v", r.Index, r.Error, want)
				}
			}
		})
	}
}

// Plan's output must not depend on map iteration order.
func TestPlanIsDeterministic(t *testing.T) {
	var items []Item
	for m := range 50 {
		for _, p := range []string{"c", "a", "b"} {
			items = append(items, you(at(11, 0).Add(time.Duration(m)*time.Minute), p, 25))
		}
	}
	first, firstRej := Plan(indexed(items...), nil, "v1", now)
	for range 20 {
		w, r := Plan(indexed(items...), nil, "v1", now)
		if fmt.Sprint(w) != fmt.Sprint(first) || fmt.Sprint(r) != fmt.Sprint(firstRej) {
			t.Fatal("Plan output changed between runs")
		}
	}
	if len(firstRej) != 50 { // the third project of every minute exceeds 60
		t.Fatalf("rejected %d, want 50", len(firstRej))
	}
}

func BenchmarkPlan(b *testing.B) {
	items := make([]Item, 0, MaxBatch)
	for i := range MaxBatch {
		items = append(items, you(at(0, 0).Add(time.Duration(i/2)*time.Minute), fmt.Sprintf("p%d", i%2), 30))
	}
	in := indexed(items...)
	b.ReportAllocs()
	for b.Loop() {
		Plan(in, nil, "v1", now)
	}
}
