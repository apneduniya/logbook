package credited

import (
	"cmp"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apneduniya/logbook/pkg/struggle"
)

// Limits and windows for minute uploads (see the package doc).
const (
	MaxBatch          = 1000
	MaxProjectLen     = 256
	MaxCodes          = 16
	MaxCreditedSecs   = 60
	FinalAfter        = 30 * time.Minute
	MaxFutureSkew     = 2 * time.Minute
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
}

// Item is a validated minute. Cuts and Flags are sorted, without duplicates
// and never nil.
type Item struct {
	Minute   time.Time
	Project  string
	Label    string
	Credited int
	Cuts     []string
	Flags    []string
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
}

// Rejection says why one item of a batch was not stored.
type Rejection struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// Item rejection reasons. They are sent to the agent, which drops rejected
// items instead of retrying them.
var (
	errMinuteFormat   = errors.New("minute must be an RFC 3339 UTC time on a whole minute")
	errMinuteFuture   = errors.New("minute is in the future")
	errMinuteTooOld   = errors.New("minute is before 2024")
	errProject        = errors.New("project must be at most 256 bytes of UTF-8 with no control characters")
	errLabel          = errors.New(`label must be "you", "agent" or "idle"`)
	errCredited       = errors.New("credited must be 0..60, and 0 unless label is \"you\"")
	errCodes          = errors.New("cuts and flags must be at most 16 codes like \"rubber_stamp\"")
	errServerFlag     = errors.New("the multi_device flag is set by the server only")
	errDuplicate      = errors.New("the same minute and project appear earlier in this batch")
	errFinal          = errors.New("minute is final: stored minutes can only be changed for 30 minutes")
	errOverMinuteCap  = errors.New("credited seconds for this minute would exceed 60 across projects")
	errUnknownVersion = errors.New("unknown rules_version")
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
	m, err := time.Parse(time.RFC3339, r.Minute)
	if err != nil {
		return Item{}, errMinuteFormat
	}
	if _, offset := m.Zone(); offset != 0 || !m.Equal(m.Truncate(time.Minute)) {
		return Item{}, errMinuteFormat
	}
	m = m.UTC()
	if m.After(now.Add(MaxFutureSkew)) {
		return Item{}, errMinuteFuture
	}
	if m.Before(earliestMinute) {
		return Item{}, errMinuteTooOld
	}
	if !validProject(r.Project) {
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
	return Item{Minute: m, Project: r.Project, Label: r.Label, Credited: r.Credited, Cuts: cuts, Flags: flags}, nil
}

// validProject allows "" (a minute with no project) and otherwise the same
// characters a heartbeat project may keep after normalisation.
func validProject(p string) bool {
	if len(p) > MaxProjectLen || !utf8.ValidString(p) {
		return false
	}
	return !strings.ContainsFunc(p, unicode.IsControl)
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

// Plan decides which validated items to write, given the device's stored
// rows for the same minutes, the batch's rules version and the server
// clock. It returns the rows to write, sorted by (minute, project), and the
// items it rejects, by the rules in the package doc. Items that repeat a
// stored row exactly are accepted without a write.
func Plan(items []Indexed, stored []Stored, rulesVersion string, now time.Time) (write []Item, rejected []Rejection) {
	existing := make(map[key]Stored, len(stored))
	total := map[time.Time]int{}
	for _, s := range stored {
		k := key{s.Minute.UTC(), s.Project}
		existing[k] = s
		total[k.minute] += s.Credited
	}

	// First pass, in input order: duplicates, exact repeats and final minutes.
	type change struct {
		Indexed
		delta int
	}
	seen := make(map[key]bool, len(items))
	byMinute := map[time.Time][]change{}
	for _, it := range items {
		k := key{it.Item.Minute, it.Item.Project}
		if seen[k] {
			rejected = append(rejected, Rejection{Index: it.Index, Error: errDuplicate.Error()})
			continue
		}
		seen[k] = true
		cur, ok := existing[k]
		if ok && cur.RulesVersion == rulesVersion && sameItem(cur.Item, it.Item) {
			continue // an exact repeat, typically a retry
		}
		if ok && now.Sub(k.minute) >= FinalAfter {
			rejected = append(rejected, Rejection{Index: it.Index, Error: errFinal.Error()})
			continue
		}
		byMinute[k.minute] = append(byMinute[k.minute], change{Indexed: it, delta: it.Item.Credited - cur.Credited})
	}

	// Second pass, per minute: apply decreases before increases so credit can
	// move between projects, and reject what would exceed the cap.
	minutes := make([]time.Time, 0, len(byMinute))
	for m := range byMinute {
		minutes = append(minutes, m)
	}
	slices.SortFunc(minutes, time.Time.Compare)
	for _, m := range minutes {
		changes := byMinute[m]
		raises := func(c change) bool { return c.delta >= 0 }
		slices.SortFunc(changes, func(a, b change) int {
			return cmp.Or(compareBool(raises(a), raises(b)), cmp.Compare(a.Index, b.Index))
		})
		for _, c := range changes {
			if total[m]+c.delta > MaxCreditedSecs {
				rejected = append(rejected, Rejection{Index: c.Index, Error: errOverMinuteCap.Error()})
				continue
			}
			total[m] += c.delta
			write = append(write, c.Item)
		}
	}

	slices.SortFunc(write, func(a, b Item) int {
		return cmp.Or(a.Minute.Compare(b.Minute), cmp.Compare(a.Project, b.Project))
	})
	slices.SortFunc(rejected, func(a, b Rejection) int { return cmp.Compare(a.Index, b.Index) })
	return write, rejected
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

func sameItem(a, b Item) bool {
	return a.Label == b.Label && a.Credited == b.Credited &&
		slices.Equal(a.Cuts, b.Cuts) && slices.Equal(a.Flags, b.Flags)
}
