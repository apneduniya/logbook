package credited

import (
	"cmp"
	"errors"
	"maps"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/apneduniya/logbook/pkg/projectname"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// Limits and windows for minute uploads (see the package doc).
const (
	MaxBatch        = 1000
	MaxProjectLen   = 256
	MaxCodes        = 16
	MaxCreditedSecs = 60
	// MaxProjectsPerMinute bounds the rows one device can store for one
	// minute, whatever their credit. It is the engine's own limit, so
	// correct engine output is never rejected for it; the check here
	// guards against other clients.
	MaxProjectsPerMinute = struggle.MaxProjectsPerMinute
	FinalAfter           = 30 * time.Minute
	// CatchUpWindow is how long after a final minute's first upload more
	// of its projects may still arrive, for an offline agent whose catch-up
	// splits a minute across batches. They must carry the key the minute's
	// rows were stored with: a device-minute is one unit (ADR 0017).
	CatchUpWindow = 5 * time.Minute
	MaxFutureSkew = 2 * time.Minute
	// KeyWindow is the length of an agent run's window: a minute computed
	// at T lies in [T - KeyWindow, T) (ADR 0017).
	KeyWindow         = 60 * time.Minute
	maxRulesVersionSz = 32
)

// earliestMinute rejects timestamps that can't come from a real agent
// (zero values, year-1 dates from broken clocks).
var earliestMinute = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// KnownRulesVersions are the rule sets this server can re-run when a
// submission is checked, so they are the only ones it stores.
var KnownRulesVersions = []string{struggle.RulesVersion}

// codeRe is the shape of a cut or flag code.
var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// RawItem is one minute as sent by the agent.
type RawItem struct {
	Minute   string   `json:"minute"`
	Project  string   `json:"project"`
	Label    string   `json:"label"`
	Credited int      `json:"credited"`
	Cuts     []string `json:"cuts"`
	Flags    []string `json:"flags"`
	// ComputedAt and Snapshot are the computation key (ADR 0017). Both are
	// required; Snapshot is a pointer so a missing one is told apart from 0.
	ComputedAt string `json:"computed_at"`
	Snapshot   *int64 `json:"snapshot"`
}

// Key is the computation key of a minute (ADR 0017): the end of the agent
// run's window (ComputedAt, a whole minute in UTC) and the high-water mark
// of the agent's event log it read (Snapshot). The zero Key marks a row
// stored before ADR 0017, which has none; an uploaded key is never zero,
// because its ComputedAt is after 2024.
type Key struct {
	ComputedAt time.Time
	Snapshot   int64
}

// IsZero reports whether k is the zero Key (a row stored without a key).
func (k Key) IsZero() bool { return k.ComputedAt.IsZero() && k.Snapshot == 0 }

// Equal reports whether k and o are the same key.
func (k Key) Equal(o Key) bool { return k.ComputedAt.Equal(o.ComputedAt) && k.Snapshot == o.Snapshot }

// Item is a validated minute. Project is normalised with
// projectname.Normalize; Cuts and Flags are sorted, without duplicates and
// never nil.
type Item struct {
	Minute   time.Time
	Project  string
	Label    string
	Credited int
	Cuts     []string
	Flags    []string
	Key      Key
}

// Indexed is a validated item with its position in the request.
type Indexed struct {
	Index int
	Item  Item
}

// Stored is a row already in credited_minutes for the device.
type Stored struct {
	Item
	RulesVersion string
	// UpdatedAt is when the row was last written, by the server clock.
	UpdatedAt time.Time
}

// Bounds is what Plan needs to know about the request and the device
// besides its stored rows.
type Bounds struct {
	// RulesVersion is the batch's rules version.
	RulesVersion string
	// Now is the server clock.
	Now time.Time
	// Since is the first minute the device may write: midnight UTC of the
	// day it was registered.
	Since time.Time
	// Sealed lists the UTC days (at midnight) for which the device has
	// stored an evidence record.
	Sealed []time.Time
}

// Rejection says why one item of a batch was not stored. Stored is set only
// when the item was refused because its minute is final or in a sealed
// day: it holds every row the server keeps for that device-minute once the
// batch is applied, sorted by project, so the agent can adopt the server's
// answer (ADR 0017). Such a refusal always sends "stored", as [] when the
// server holds no row; other refusals leave it out (nil).
type Rejection struct {
	Index  int         `json:"index"`
	Error  string      `json:"error"`
	Stored []StoredRow `json:"stored,omitzero"`
}

// StoredRow is one stored row of a final minute, as returned in a
// rejection. ComputedAt and Snapshot are null for a row stored before
// ADR 0017.
type StoredRow struct {
	Project    string     `json:"project"`
	Label      string     `json:"label"`
	Credited   int        `json:"credited"`
	Cuts       []string   `json:"cuts"`
	Flags      []string   `json:"flags"`
	ComputedAt *time.Time `json:"computed_at"`
	Snapshot   *int64     `json:"snapshot"`
}

func storedRow(it Item) StoredRow {
	r := StoredRow{Project: it.Project, Label: it.Label, Credited: it.Credited, Cuts: it.Cuts, Flags: it.Flags}
	if !it.Key.IsZero() {
		at, snap := it.Key.ComputedAt.UTC(), it.Key.Snapshot
		r.ComputedAt, r.Snapshot = &at, &snap
	}
	return r
}

// Item rejection reasons. They are sent to the agent, which drops rejected
// items instead of retrying them.
var (
	errMinuteFormat    = errors.New("minute must be an RFC 3339 UTC time on a whole minute")
	errMinuteFuture    = errors.New("minute is in the future")
	errMinuteTooOld    = errors.New("minute is before 2024")
	errComputedAt      = errors.New("computed_at must be an RFC 3339 UTC time on a whole minute")
	errKeyWindow       = errors.New("minute must be in the 60 minutes before computed_at")
	errSnapshot        = errors.New("snapshot must be an integer >= 0")
	errProject         = errors.New("project must be at most 256 bytes of UTF-8")
	errLabel           = errors.New(`label must be "you", "agent" or "idle"`)
	errCredited        = errors.New("credited must be 0..60, and 0 unless label is \"you\"")
	errCodes           = errors.New("cuts and flags must be at most 16 codes like \"rubber_stamp\"")
	errServerFlag      = errors.New("the multi_device flag is set by the server only")
	errDuplicate       = errors.New("the same minute and project appear earlier in this batch")
	errBeforeDevice    = errors.New("minute is before the UTC day the device was registered")
	errSealed          = errors.New("minute is in a UTC day this device has already sealed with an evidence record")
	errFinal           = errors.New("minute is final: stored minutes can only be changed for 30 minutes")
	errMixedKeys       = errors.New("all rows of a minute must carry the same computed_at and snapshot")
	errTooManyProjects = errors.New("a minute can have at most 8 projects per device")
	errOverMinuteCap   = errors.New("credited seconds for this minute would exceed 60 across projects")
	errUnknownVersion  = errors.New("unknown rules_version")
)

// ValidRulesVersion reports whether v is one of KnownRulesVersions.
func ValidRulesVersion(v string) bool {
	return len(v) <= maxRulesVersionSz && slices.Contains(KnownRulesVersions, v)
}

// Parse validates each raw item against now (the server clock). Valid items
// come back in input order; the rest are rejected with a reason.
func Parse(raw []RawItem, now time.Time) ([]Indexed, []Rejection) {
	valid := make([]Indexed, 0, len(raw))
	var rejected []Rejection
	for i, r := range raw {
		it, err := parseItem(r, now)
		if err != nil {
			rejected = append(rejected, Rejection{Index: i, Error: err.Error()})
			continue
		}
		valid = append(valid, Indexed{Index: i, Item: it})
	}
	return valid, rejected
}

func parseItem(r RawItem, now time.Time) (Item, error) {
	m, ok := parseMinute(r.Minute)
	if !ok {
		return Item{}, errMinuteFormat
	}
	if m.After(now.Add(MaxFutureSkew)) {
		return Item{}, errMinuteFuture
	}
	if m.Before(earliestMinute) {
		return Item{}, errMinuteTooOld
	}
	if !utf8.ValidString(r.Project) {
		return Item{}, errProject
	}
	// The same normalisation as heartbeat projects, so credited and raw
	// totals use the same names ("" means no project).
	project := projectname.Normalize(r.Project)
	if len(project) > MaxProjectLen {
		return Item{}, errProject
	}
	switch struggle.Label(r.Label) {
	case struggle.LabelYou, struggle.LabelAgent, struggle.LabelIdle:
	default:
		return Item{}, errLabel
	}
	if r.Credited < 0 || r.Credited > MaxCreditedSecs || (r.Credited > 0 && r.Label != string(struggle.LabelYou)) {
		return Item{}, errCredited
	}
	cuts, err := normalizeCodes(r.Cuts)
	if err != nil {
		return Item{}, err
	}
	flags, err := normalizeCodes(r.Flags)
	if err != nil {
		return Item{}, err
	}
	if slices.Contains(flags, struggle.FlagMultiDevice) {
		return Item{}, errServerFlag
	}
	key, err := parseKey(r, m)
	if err != nil {
		return Item{}, err
	}
	return Item{Minute: m, Project: project, Label: r.Label, Credited: r.Credited, Cuts: cuts, Flags: flags, Key: key}, nil
}

// parseMinute parses an RFC 3339 time in UTC on a whole minute.
func parseMinute(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	if _, offset := t.Zone(); offset != 0 || !t.Equal(t.Truncate(time.Minute)) {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// parseKey checks the computation key of minute m: a run whose window ends
// at computed_at covers the KeyWindow before it (ADR 0017).
func parseKey(r RawItem, m time.Time) (Key, error) {
	at, ok := parseMinute(r.ComputedAt)
	if !ok {
		return Key{}, errComputedAt
	}
	if !m.Before(at) || m.Before(at.Add(-KeyWindow)) {
		return Key{}, errKeyWindow
	}
	if r.Snapshot == nil || *r.Snapshot < 0 {
		return Key{}, errSnapshot
	}
	return Key{ComputedAt: at, Snapshot: *r.Snapshot}, nil
}

// normalizeCodes checks codes and returns them sorted and deduplicated.
func normalizeCodes(codes []string) ([]string, error) {
	if len(codes) > MaxCodes {
		return nil, errCodes
	}
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		if !codeRe.MatchString(c) {
			return nil, errCodes
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// key identifies a row of one device.
type key struct {
	minute  time.Time
	project string
}

// minuteState summarises a device's stored rows for one minute.
type minuteState struct {
	credited   int       // sum of credited seconds
	projects   int       // number of rows
	firstWrite time.Time // earliest updated_at
	key        Key       // the rows' computation key, when they share one
	oneKey     bool      // every row carries key
}

// IsRemoval reports whether it removes its (minute, project) row instead of
// storing one: an idle item with no credit, cuts or flags. The engine never
// emits such a row (its idle rows always carry a cut), and it says nothing
// a missing row doesn't, so the agent sends it for a project that has left
// an open minute (ADR 0014).
func IsRemoval(it Item) bool {
	return it.Label == string(struggle.LabelIdle) && it.Credited == 0 && len(it.Cuts) == 0 && len(it.Flags) == 0
}

// Plan decides which validated items to write and which stored rows to
// delete, given the device's stored rows for the same minutes and b. It
// returns the rows to write and the rows to delete, both sorted by (minute,
// project), and the items it rejects, by the rules in the package doc.
// Items that repeat a stored row exactly are accepted without a write, and
// a removal of a row that isn't stored is accepted without a delete.
func Plan(items []Indexed, stored []Stored, b Bounds) (write, remove []Item, rejected []Rejection) {
	existing := make(map[key]Stored, len(stored))
	minutes := map[time.Time]minuteState{}
	for _, s := range stored {
		k := key{s.Minute.UTC(), s.Project}
		existing[k] = s
		st := minutes[k.minute]
		if st.projects == 0 || s.UpdatedAt.Before(st.firstWrite) {
			st.firstWrite = s.UpdatedAt
		}
		if st.projects == 0 {
			st.key, st.oneKey = s.Key, true
		} else if !st.key.Equal(s.Key) {
			st.oneKey = false
		}
		st.credited += s.Credited
		st.projects++
		minutes[k.minute] = st
	}
	sealed := make(map[time.Time]bool, len(b.Sealed))
	for _, d := range b.Sealed {
		sealed[utcDay(d)] = true
	}

	// First pass, in input order: duplicates, exact repeats, removals of
	// nothing and minutes the device may no longer write.
	type change struct {
		Indexed
		delta  int
		isNew  bool // adds a project to the minute
		remove bool // deletes the stored row
	}
	seen := make(map[key]bool, len(items))
	byMinute := map[time.Time][]change{}
	finalAt := map[int]time.Time{} // minute of each item refused as final, by index
	for _, it := range items {
		k := key{it.Item.Minute, it.Item.Project}
		if seen[k] {
			rejected = append(rejected, Rejection{Index: it.Index, Error: errDuplicate.Error()})
			continue
		}
		seen[k] = true
		cur, ok := existing[k]
		removal := IsRemoval(it.Item)
		err := writable(it.Item, ok, minutes[k.minute], sealed, b)
		if removal && !ok {
			if err == nil || minutes[k.minute].projects == 0 {
				continue // nothing stored, nothing to remove
			}
			// The device asks to remove a row the server doesn't hold, in a
			// minute it can no longer change but holds other rows for: the
			// device's view of the minute is wrong, so it gets the minute's
			// rows back below instead of a silent success.
		}
		if ok && cur.RulesVersion == b.RulesVersion && sameItem(cur.Item, it.Item) && (!removal || err != nil) {
			// An exact repeat, typically a retry. A row stored in the shape
			// of a removal (by a server before removals) is still removed
			// while its minute is open.
			continue
		}
		if err != nil {
			rejected = append(rejected, Rejection{Index: it.Index, Error: err.Error()})
			if errors.Is(err, errFinal) || errors.Is(err, errSealed) {
				finalAt[it.Index] = k.minute
			}
			continue
		}
		byMinute[k.minute] = append(byMinute[k.minute], change{Indexed: it, delta: it.Item.Credited - cur.Credited,
			isNew: !ok, remove: removal})
	}

	// Second pass, per minute: apply removals, then decreases, then the rest
	// in request order, so a project can replace another and credit can move
	// between projects in one batch; reject what would exceed the caps.
	order := make([]time.Time, 0, len(byMinute))
	for m := range byMinute {
		order = append(order, m)
	}
	slices.SortFunc(order, time.Time.Compare)
	for _, m := range order {
		changes := byMinute[m]
		rank := func(c change) int {
			switch {
			case c.remove:
				return 0
			case c.delta < 0:
				return 1
			}
			return 2
		}
		slices.SortFunc(changes, func(a, b change) int {
			return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Index, b.Index))
		})
		st := minutes[m]
		var mw, mr []Item
		var mrej []Rejection
		for _, c := range changes {
			if c.remove {
				st.credited += c.delta
				st.projects--
				mr = append(mr, c.Item)
				continue
			}
			if c.isNew && st.projects >= MaxProjectsPerMinute {
				mrej = append(mrej, Rejection{Index: c.Index, Error: errTooManyProjects.Error()})
				continue
			}
			if st.credited+c.delta > MaxCreditedSecs {
				mrej = append(mrej, Rejection{Index: c.Index, Error: errOverMinuteCap.Error()})
				continue
			}
			st.credited += c.delta
			if c.isNew {
				st.projects++
			}
			mw = append(mw, c.Item)
		}
		// A device-minute is one unit that a single key reproduces (ADR
		// 0017): the agent replaces a changed minute whole, so every row it
		// keeps afterwards, and every removal it sends with them, carries
		// the same key. A batch that would leave rows of two computations
		// (two keys in the request, a new key next to untouched rows of an
		// older one, or a removal from another computation than the rows
		// it leaves) changes nothing in the minute.
		after := held(m, stored, mw, mr)
		for _, it := range mr {
			after = append(after, storedRow(it))
		}
		if !oneKey(after) {
			for _, c := range changes {
				rejected = append(rejected, Rejection{Index: c.Index, Error: errMixedKeys.Error()})
			}
			continue
		}
		write = append(write, mw...)
		remove = append(remove, mr...)
		rejected = append(rejected, mrej...)
		minutes[m] = st
	}

	byKey := func(a, b Item) int {
		return cmp.Or(a.Minute.Compare(b.Minute), cmp.Compare(a.Project, b.Project))
	}
	slices.SortFunc(write, byKey)
	slices.SortFunc(remove, byKey)
	slices.SortFunc(rejected, func(a, b Rejection) int { return cmp.Compare(a.Index, b.Index) })
	for i, r := range rejected {
		if m, ok := finalAt[r.Index]; ok {
			rejected[i].Stored = held(m, stored, write, remove)
		}
	}
	return write, remove, rejected
}

// held returns the rows the device holds for minute m once the batch's
// writes and removals are applied, sorted by project. A final minute can
// still gain a project in the same batch during an offline catch-up, so the
// stored rows alone may be out of date.
func held(m time.Time, stored []Stored, write, remove []Item) []StoredRow {
	rows := map[string]Item{}
	for _, s := range stored {
		if s.Minute.Equal(m) {
			rows[s.Project] = s.Item
		}
	}
	for _, it := range remove {
		if it.Minute.Equal(m) {
			delete(rows, it.Project)
		}
	}
	for _, it := range write {
		if it.Minute.Equal(m) {
			rows[it.Project] = it
		}
	}
	out := make([]StoredRow, 0, len(rows))
	for _, p := range slices.Sorted(maps.Keys(rows)) {
		out = append(out, storedRow(rows[p]))
	}
	return out
}

// oneKey reports whether every row carries the same key (legacy rows have
// the zero key).
func oneKey(rows []StoredRow) bool {
	for _, r := range rows[min(1, len(rows)):] {
		if !sameKey(r, rows[0]) {
			return false
		}
	}
	return true
}

// writable reports why the device may not write it (nil if it may).
// exists says whether its (minute, project) is stored; st describes all of
// the minute's stored rows.
func writable(it Item, exists bool, st minuteState, sealed map[time.Time]bool, b Bounds) error {
	m := it.Minute
	switch {
	case m.Before(b.Since):
		return errBeforeDevice
	case sealed[utcDay(m)]:
		return errSealed
	case b.Now.Sub(m) < FinalAfter:
		return nil // still open
	case exists:
		return errFinal
	case st.projects == 0:
		return nil // first upload of a final minute: offline catch-up
	}
	// A final minute with stored rows takes new projects only while it is
	// being caught up: every row was first written after the minute became
	// final, and the first of them less than CatchUpWindow ago. A minute
	// uploaded while open is closed for good, so idle or agent time can't
	// turn into "you" time later under another project. The new project
	// must carry the key of the stored rows: it is the rest of the same
	// computation, so the device-minute stays one unit that a single key
	// reproduces (ADR 0017). A row from another computation is refused like
	// a rewrite.
	catchingUp := !st.firstWrite.Before(m.Add(FinalAfter)) && b.Now.Sub(st.firstWrite) < CatchUpWindow
	if catchingUp && st.oneKey && !st.key.IsZero() && st.key.Equal(it.Key) {
		return nil
	}
	return errFinal
}

// utcDay returns midnight UTC of t's UTC day.
func utcDay(t time.Time) time.Time {
	y, mo, d := t.UTC().Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
}

// sameItem reports whether a and b store the same row, key included.
func sameItem(a, b Item) bool {
	return a.Label == b.Label && a.Credited == b.Credited &&
		slices.Equal(a.Cuts, b.Cuts) && slices.Equal(a.Flags, b.Flags) && a.Key.Equal(b.Key)
}

// sameKey reports whether a and b carry the same key (both legacy counts).
func sameKey(a, b StoredRow) bool {
	if (a.ComputedAt == nil) != (b.ComputedAt == nil) || (a.Snapshot == nil) != (b.Snapshot == nil) {
		return false
	}
	return (a.ComputedAt == nil || a.ComputedAt.Equal(*b.ComputedAt)) && (a.Snapshot == nil || *a.Snapshot == *b.Snapshot)
}
