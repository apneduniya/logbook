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

func ptr[T any](v T) *T { return &v }

// keyed gives r the key of a run that ended a minute after r's minute,
// unless r has a computed_at or a snapshot of its own.
func keyed(r RawItem) RawItem {
	if r.ComputedAt != "" || r.Snapshot != nil {
		return r
	}
	if m, err := time.Parse(time.RFC3339, r.Minute); err == nil {
		r.ComputedAt = m.UTC().Truncate(time.Minute).Add(time.Minute).Format(time.RFC3339)
	}
	r.Snapshot = ptr(int64(7))
	return r
}

func TestParse(t *testing.T) {
	ok := RawItem{Minute: "2026-11-20T11:59:00Z", Project: "logbook", Label: "you", Credited: 45}
	with := func(f func(*RawItem)) RawItem { r := ok; f(&r); return r }
	key := func(computedAt string, snapshot *int64) RawItem {
		return with(func(r *RawItem) { r.ComputedAt, r.Snapshot = computedAt, snapshot })
	}
	tests := []struct {
		name    string
		raw     RawItem
		wantErr error
	}{
		{"computed a minute later", key("2026-11-20T12:00:00Z", ptr(int64(1))), nil},
		{"computed an hour later", key("2026-11-20T12:59:00Z", ptr(int64(1))), nil},
		{"snapshot 0", key("2026-11-20T12:00:00Z", ptr(int64(0))), nil},
		{"computed more than an hour later", key("2026-11-20T13:00:00Z", ptr(int64(1))), errKeyWindow},
		{"computed at the minute", key("2026-11-20T11:59:00Z", ptr(int64(1))), errKeyWindow},
		{"computed before the minute", key("2026-11-20T11:00:00Z", ptr(int64(1))), errKeyWindow},
		{"computed_at with seconds", key("2026-11-20T12:00:30Z", ptr(int64(1))), errComputedAt},
		{"computed_at with offset", key("2026-11-20T17:30:00+05:30", ptr(int64(1))), errComputedAt},
		{"computed_at not a time", key("soon", ptr(int64(1))), errComputedAt},
		{"no computed_at", key("", ptr(int64(1))), errComputedAt},
		{"no snapshot", key("2026-11-20T12:00:00Z", nil), errSnapshot},
		{"negative snapshot", key("2026-11-20T12:00:00Z", ptr(int64(-1))), errSnapshot},
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
		{"control char in project is dropped", with(func(r *RawItem) { r.Project = "a\nb" }), nil},
		{"256 bytes after trimming", with(func(r *RawItem) { r.Project = strings.Repeat("a", 256) + " " }), nil},
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
			valid, rejected := Parse([]RawItem{keyed(tc.raw)}, now)
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

// Projects are normalised like heartbeat projects, so credited and raw
// totals group under the same names.
func TestParseNormalizesProjects(t *testing.T) {
	tests := []struct{ in, want string }{
		{"logbook", "logbook"},
		{" logbook\t", "logbook"},
		{"<<LAST_PROJECT>>", ""},
		{"log\x00book", "logbook"},
		{"", ""},
	}
	for _, tc := range tests {
		valid, rejected := Parse([]RawItem{keyed(RawItem{Minute: "2026-11-20T11:59:00Z", Project: tc.in, Label: "idle"})}, now)
		if len(rejected) != 0 || valid[0].Item.Project != tc.want {
			t.Errorf("Parse(project %q) = %+v %+v, want project %q", tc.in, valid, rejected, tc.want)
		}
	}
	// Two spellings of one project in a batch are a duplicate after
	// normalisation.
	valid, _ := Parse([]RawItem{
		keyed(RawItem{Minute: "2026-11-20T11:59:00Z", Project: "logbook", Label: "you", Credited: 10}),
		keyed(RawItem{Minute: "2026-11-20T11:59:00Z", Project: "logbook ", Label: "you", Credited: 10}),
	}, now)
	_, _, rejected := Plan(valid, nil, bounds())
	if len(rejected) != 1 || rejected[0].Index != 1 || rejected[0].Error != errDuplicate.Error() {
		t.Fatalf("rejected = %+v", rejected)
	}
}

func TestParseNormalizesCodes(t *testing.T) {
	valid, _ := Parse([]RawItem{keyed(RawItem{
		Minute: "2026-11-20T11:59:00Z", Label: "you", Credited: 0,
		Cuts: []string{"rubber_stamp", "loop", "rubber_stamp"}, Flags: nil,
	})}, now)
	it := valid[0].Item
	if !slices.Equal(it.Cuts, []string{"loop", "rubber_stamp"}) || it.Flags == nil || len(it.Flags) != 0 {
		t.Fatalf("cuts %q flags %#v", it.Cuts, it.Flags)
	}
}

// you is a "you" item computed by a run that ended a minute after it.
func you(minute time.Time, project string, credited int) Item {
	return Item{Minute: minute, Project: project, Label: "you", Credited: credited, Cuts: []string{}, Flags: []string{},
		Key: Key{ComputedAt: minute.Add(time.Minute), Snapshot: 7}}
}

// stored is a row uploaded in real time: written a minute after its minute.
func stored(it Item) Stored {
	return Stored{Item: it, RulesVersion: "v1", UpdatedAt: it.Minute.Add(time.Minute)}
}

// caughtUp is a row first uploaded at written, by an agent catching up.
func caughtUp(it Item, written time.Time) Stored {
	return Stored{Item: it, RulesVersion: "v1", UpdatedAt: written}
}

// idle is an idle row as the engine emits it: with a cut.
func idle(minute time.Time, project string) Item {
	it := you(minute, project, 0)
	it.Label, it.Cuts = "idle", []string{"idle"}
	return it
}

// removal is the item that removes a project from an open minute.
func removal(minute time.Time, project string) Item {
	it := you(minute, project, 0)
	it.Label = "idle"
	return it
}

// bounds are the default Bounds: a device registered long before the test
// minutes, with no sealed days.
func bounds() Bounds {
	return Bounds{RulesVersion: "v1", Now: now, Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

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
			name:    "a final minute uploaded in real time takes no new project",
			stored:  []Stored{stored(idle(old, "a"))},
			items:   []Item{you(old, "b", 60)},
			wantRej: map[int]error{0: errFinal},
		},
		{
			name:    "a minute that turned final takes no new project either",
			stored:  []Stored{stored(idle(final, "a"))},
			items:   []Item{you(final, "b", 60)},
			wantRej: map[int]error{0: errFinal},
		},
		{
			name:      "a caught-up final minute takes more projects for a short while",
			stored:    []Stored{caughtUp(you(old, "a", 30), now.Add(-2*time.Minute))},
			items:     []Item{you(old, "b", 30)},
			wantWrite: []string{"09:00 b 30"},
		},
		{
			name:    "a caught-up final minute closes after the catch-up window",
			stored:  []Stored{caughtUp(you(old, "a", 30), now.Add(-CatchUpWindow))},
			items:   []Item{you(old, "b", 30)},
			wantRej: map[int]error{0: errFinal},
		},
		{
			name:    "a caught-up row can't be changed",
			stored:  []Stored{caughtUp(you(old, "a", 30), now.Add(-time.Minute))},
			items:   []Item{you(old, "a", 60)},
			wantRej: map[int]error{0: errFinal},
		},
		{
			name: "a minute caught up partly in real time stays closed",
			stored: []Stored{
				stored(idle(old, "a")),
				caughtUp(you(old, "b", 10), now.Add(-time.Minute)),
			},
			items:   []Item{you(old, "c", 30)},
			wantRej: map[int]error{0: errFinal},
		},
		{
			name: "at most 8 projects per minute",
			stored: []Stored{
				stored(idle(fresh, "p1")), stored(idle(fresh, "p2")), stored(idle(fresh, "p3")),
				stored(idle(fresh, "p4")), stored(idle(fresh, "p5")), stored(idle(fresh, "p6")),
				stored(idle(fresh, "p7")),
			},
			items:     []Item{idle(fresh, "p8"), idle(fresh, "p9"), you(fresh, "p1", 10)},
			wantWrite: []string{"11:50 p1 10", "11:50 p8 0"},
			wantRej:   map[int]error{1: errTooManyProjects},
		},
		{
			name: "a batch alone can't exceed 8 projects per minute",
			items: []Item{
				idle(old, "p1"), idle(old, "p2"), idle(old, "p3"), idle(old, "p4"), idle(old, "p5"),
				idle(old, "p6"), idle(old, "p7"), idle(old, "p8"), idle(old, "p9"),
			},
			wantWrite: []string{
				"09:00 p1 0", "09:00 p2 0", "09:00 p3 0", "09:00 p4 0",
				"09:00 p5 0", "09:00 p6 0", "09:00 p7 0", "09:00 p8 0",
			},
			wantRej: map[int]error{8: errTooManyProjects},
		},
		{
			name:    "an item over the credit cap doesn't take a project slot",
			stored:  []Stored{stored(you(fresh, "a", 60))},
			items:   []Item{you(fresh, "b", 10)},
			wantRej: map[int]error{0: errOverMinuteCap},
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
			stored:  []Stored{stored(you(fresh, "a", 50))},
			items:   []Item{you(fresh, "b", 20)},
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
			write, _, rejected := Plan(indexed(tc.items...), tc.stored, bounds())
			checkPlan(t, write, rejected, tc.wantWrite, tc.wantRej)
		})
	}
}

func checkPlan(t *testing.T, write []Item, rejected []Rejection, wantWrite []string, wantRej map[int]error) {
	t.Helper()
	got := make([]string, len(write))
	for i, w := range write {
		got[i] = fmt.Sprintf("%s %s %d", w.Minute.Format("15:04"), w.Project, w.Credited)
	}
	if !slices.Equal(got, wantWrite) && (len(got) != 0 || len(wantWrite) != 0) {
		t.Errorf("write = %q, want %q", got, wantWrite)
	}
	if len(rejected) != len(wantRej) {
		t.Fatalf("rejected = %+v, want %v", rejected, wantRej)
	}
	for _, r := range rejected {
		if want, ok := wantRej[r.Index]; !ok || r.Error != want.Error() {
			t.Errorf("rejected[%d] = %q, want %v", r.Index, r.Error, want)
		}
	}
}

// A removal deletes its row from an open minute before anything else in
// the batch is applied, so a replacement fits under the project limit and
// the credit cap.
func TestPlanRemovals(t *testing.T) {
	fresh, final := at(11, 50), at(11, 30)
	eight := func(m time.Time) []Stored {
		var out []Stored
		for _, p := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			out = append(out, stored(you(m, p, 7)))
		}
		out[0].Credited = 11
		return out
	}
	tests := []struct {
		name       string
		stored     []Stored
		items      []Item
		wantWrite  []string
		wantRemove []string
		wantRej    map[int]error
	}{
		{
			name:       "a removal frees a slot for a replacement listed before it",
			stored:     eight(fresh),
			items:      []Item{you(fresh, "i", 7), removal(fresh, "h")},
			wantWrite:  []string{"11:50 i 7"},
			wantRemove: []string{"11:50 h 0"},
		},
		{
			name:       "a removal frees credit for a raise",
			stored:     eight(fresh),
			items:      []Item{you(fresh, "a", 18), removal(fresh, "h")},
			wantWrite:  []string{"11:50 a 18"},
			wantRemove: []string{"11:50 h 0"},
		},
		{
			name:    "a removal of nothing frees nothing",
			stored:  eight(fresh),
			items:   []Item{removal(fresh, "x"), you(fresh, "i", 0)},
			wantRej: map[int]error{1: errTooManyProjects},
		},
		{
			name:    "a removal in a final minute is rejected",
			stored:  eight(final),
			items:   []Item{removal(final, "h"), you(final, "i", 7)},
			wantRej: map[int]error{0: errFinal, 1: errFinal},
		},
		{
			name:       "a stored row shaped like a removal is removed while open",
			stored:     []Stored{stored(removal(fresh, "h"))},
			items:      []Item{removal(fresh, "h")},
			wantRemove: []string{"11:50 h 0"},
		},
		{
			name:   "and is an exact repeat once final",
			stored: []Stored{stored(removal(final, "h"))},
			items:  []Item{removal(final, "h")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			write, remove, rejected := Plan(indexed(tc.items...), tc.stored, bounds())
			checkPlan(t, write, rejected, tc.wantWrite, tc.wantRej)
			got := make([]string, len(remove))
			for i, r := range remove {
				got[i] = fmt.Sprintf("%s %s %d", r.Minute.Format("15:04"), r.Project, r.Credited)
			}
			if !slices.Equal(got, tc.wantRemove) && (len(got) != 0 || len(tc.wantRemove) != 0) {
				t.Errorf("remove = %q, want %q", got, tc.wantRemove)
			}
		})
	}
}

// rekey returns it computed at a later snapshot.
func rekey(it Item, snapshot int64) Item {
	it.Key.Snapshot = snapshot
	return it
}

// legacy is a row stored before ADR 0017, without a key.
func legacy(it Item) Stored {
	it.Key = Key{}
	return stored(it)
}

// fmtHeld writes stored rows as "project label credited computed_at/snapshot".
func fmtHeld(rows []StoredRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		key := "null/null"
		if r.ComputedAt != nil && r.Snapshot != nil {
			key = fmt.Sprintf("%s/%d", r.ComputedAt.Format("15:04"), *r.Snapshot)
		}
		out[i] = fmt.Sprintf("%s %s %d %s", r.Project, r.Label, r.Credited, key)
	}
	return out
}

// A changed key is a rewrite: accepted while the minute is open, refused
// once it is final, with the rows the server holds (ADR 0017).
func TestPlanKeys(t *testing.T) {
	fresh, final, old := at(11, 50), at(11, 30), at(9, 0)
	tests := []struct {
		name      string
		stored    []Stored
		items     []Item
		wantWrite []string
		wantRej   map[int]error
		wantHeld  map[int][]string // index → held rows of a "final" rejection
	}{
		{
			name:      "a new key on an open minute is written",
			stored:    []Stored{stored(you(fresh, "a", 30))},
			items:     []Item{rekey(you(fresh, "a", 30), 9)},
			wantWrite: []string{"11:50 a 30"},
		},
		{
			name:     "a new key on a final minute is refused with the held rows",
			stored:   []Stored{stored(you(final, "b", 20)), stored(you(final, "a", 30))},
			items:    []Item{rekey(you(final, "a", 30), 9)},
			wantRej:  map[int]error{0: errFinal},
			wantHeld: map[int][]string{0: {"a you 30 11:31/7", "b you 20 11:31/7"}},
		},
		{
			name:   "the exact stored row, key included, is a repeat once final",
			stored: []Stored{stored(you(old, "a", 30))},
			items:  []Item{you(old, "a", 30)},
		},
		{
			name:     "a row stored without a key is returned with a null key",
			stored:   []Stored{legacy(you(old, "a", 30))},
			items:    []Item{you(old, "a", 30)},
			wantRej:  map[int]error{0: errFinal},
			wantHeld: map[int][]string{0: {"a you 30 null/null"}},
		},
		{
			name:      "a row stored without a key gains one while open",
			stored:    []Stored{legacy(you(fresh, "a", 30))},
			items:     []Item{you(fresh, "a", 30)},
			wantWrite: []string{"11:50 a 30"},
		},
		{
			name:      "held rows include a project the same batch caught up",
			stored:    []Stored{caughtUp(you(old, "a", 30), now.Add(-time.Minute))},
			items:     []Item{rekey(you(old, "a", 30), 9), you(old, "b", 30)},
			wantWrite: []string{"09:00 b 30"},
			wantRej:   map[int]error{0: errFinal},
			wantHeld:  map[int][]string{0: {"a you 30 09:01/7", "b you 30 09:01/7"}},
		},
		{
			name:    "other rejections carry no rows",
			stored:  []Stored{stored(you(fresh, "a", 60))},
			items:   []Item{you(fresh, "b", 10)},
			wantRej: map[int]error{0: errOverMinuteCap},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			write, _, rejected := Plan(indexed(tc.items...), tc.stored, bounds())
			checkPlan(t, write, rejected, tc.wantWrite, tc.wantRej)
			for _, r := range rejected {
				if got, want := fmtHeld(r.Stored), tc.wantHeld[r.Index]; !slices.Equal(got, want) {
					t.Errorf("rejected[%d].Stored = %q, want %q", r.Index, got, want)
				}
			}
		})
	}
}

// A device writes minutes from the UTC day it was registered on, and never
// into a day it has sealed with an evidence record.
func TestPlanBounds(t *testing.T) {
	yesterday := func(h, m int) time.Time { return at(h, m).AddDate(0, 0, -1) }
	registered := at(8, 15) // registered at 08:15 today: today's earlier minutes are fine
	tests := []struct {
		name      string
		since     time.Time
		sealed    []time.Time
		stored    []Stored
		items     []Item
		wantWrite []string
		wantRej   map[int]error
	}{
		{
			name:      "minutes from the registration day on",
			since:     utcDay(registered),
			items:     []Item{you(yesterday(23, 59), "a", 60), you(at(0, 0), "a", 60), you(at(11, 50), "a", 60)},
			wantWrite: []string{"00:00 a 60", "11:50 a 60"},
			wantRej:   map[int]error{0: errBeforeDevice},
		},
		{
			name:      "a sealed day takes no new minutes",
			since:     utcDay(yesterday(0, 0)),
			sealed:    []time.Time{utcDay(yesterday(0, 0))},
			items:     []Item{you(yesterday(10, 0), "a", 60), you(at(9, 0), "a", 60)},
			wantWrite: []string{"09:00 a 60"},
			wantRej:   map[int]error{0: errSealed},
		},
		{
			name:   "an exact repeat in a sealed day is still accepted",
			since:  utcDay(yesterday(0, 0)),
			sealed: []time.Time{utcDay(yesterday(0, 0))},
			stored: []Stored{stored(you(yesterday(10, 0), "a", 60))},
			items:  []Item{you(yesterday(10, 0), "a", 60)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := bounds()
			b.Since, b.Sealed = tc.since, tc.sealed
			write, _, rejected := Plan(indexed(tc.items...), tc.stored, b)
			checkPlan(t, write, rejected, tc.wantWrite, tc.wantRej)
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
	first, _, firstRej := Plan(indexed(items...), nil, bounds())
	for range 20 {
		w, _, r := Plan(indexed(items...), nil, bounds())
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
		Plan(in, nil, bounds())
	}
}
