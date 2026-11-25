package submission

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
)

// Limits of a submission (docs/api/submissions.md).
const (
	// MaxRangeDays is the longest range, in whole UTC days.
	MaxRangeDays = 92
	// MaxCompressedBytes and MaxDecompressedBytes bound an upload body.
	MaxCompressedBytes   = 32 << 20
	MaxDecompressedBytes = 256 << 20
	// MaxMismatches is how many mismatched minutes a packet lists.
	MaxMismatches = 200
	// BundleFormat names the verification bundle's format (Bundle.Format).
	BundleFormat = "logbook-submission-bundle-v1"
)

// ErrMalformed is wrapped by every reason a part, upload or bundle is
// invalid: a submission whose part is malformed fails (status failed), it
// doesn't leave minutes unbacked.
var ErrMalformed = errors.New("malformed submission")

// Plan is the answer to GET /api/v1/me/submissions/plan: the minute rows
// the server holds for the project and range, from every device.
type Plan struct {
	Project string       `json:"project"`
	From    string       `json:"from"`
	To      string       `json:"to"`
	Devices []PlanDevice `json:"devices"`
	// Minutes are sorted by (device, minute).
	Minutes []Minute `json:"minutes"`
}

// PlanDevice is a device with rows in a plan.
type PlanDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// SealedThrough is the last UTC day the device has sealed, or nil when
	// it has sealed none.
	SealedThrough *string `json:"sealed_through"`
	// ThisDevice is true for the device the plan's optional device query
	// named.
	ThisDevice bool `json:"this_device"`
}

// Minute is one credited-minute row of a device (ADR 0014) as the server
// holds it: in a plan, and frozen in a submission (submission_minutes).
type Minute struct {
	DeviceID     string    `json:"device_id"`
	Minute       time.Time `json:"minute"`
	Project      string    `json:"project"`
	Label        string    `json:"label"`
	Credited     int       `json:"credited"`
	Cuts         []string  `json:"cuts"`
	Flags        []string  `json:"flags"`
	RulesVersion string    `json:"rules_version"`
	// ComputedAt and Snapshot are the row's computation key (T, H) (ADR
	// 0017), both nil for a row stored before it, which can't be backed.
	ComputedAt *time.Time `json:"computed_at"`
	Snapshot   *int64     `json:"snapshot"`
}

// Key returns the row's computation key, and false when it has none.
func (m Minute) Key() (replay.Key, bool) {
	if m.ComputedAt == nil || m.Snapshot == nil {
		return replay.Key{}, false
	}
	return replay.Key{T: m.ComputedAt.UTC(), H: *m.Snapshot}, true
}

// Upload is the body of POST /api/v1/submissions.
type Upload struct {
	Project string `json:"project"`
	From    string `json:"from"`
	To      string `json:"to"`
	// Program is a public program's slug, or "" for none (ADR 0021).
	Program string `json:"program,omitempty"`
	Part    Part   `json:"part"`
}

// Part is one device's evidence for a submission: the body of POST
// /api/v1/submissions/{id}/parts, and Upload.Part.
type Part struct {
	DeviceID string `json:"device_id"`
	// Blocks are proofs (ADR 0019 point 5) of the device's sealed blocks,
	// one per day at most.
	Blocks []Block `json:"blocks"`
	// Minutes are the minutes this part backs, by their key. A frozen row
	// of the device not listed here is unbacked.
	Minutes []PartMinute `json:"minutes"`
}

// Block is the proof of one sealed block, named by its evidence day. Its
// JSON is the day next to the proof's fields: {"day", "buckets", "opened",
// "edges", "hashes"}.
type Block struct {
	Day string `json:"day"`
	evidence.Proof
}

// PartMinute names a minute a part backs, with its computation key.
type PartMinute struct {
	Minute     time.Time `json:"minute"`
	ComputedAt time.Time `json:"computed_at"`
	Snapshot   int64     `json:"snapshot"`
}

// Key returns the minute's computation key.
func (m PartMinute) Key() replay.Key { return replay.Key{T: m.ComputedAt.UTC(), H: m.Snapshot} }

// EvidenceRecord is one signed evidence day of a device as the server
// stores it, in the JSON of POST /api/v1/devices/{id}/evidence-days.
type EvidenceRecord struct {
	Day           string `json:"day"`
	PrevChainHash string `json:"prev_chain_hash"`
	ContentHash   string `json:"content_hash"`
	ChainHash     string `json:"chain_hash"`
	// Signature is the ed25519 signature over the chain hash (standard
	// base64 in JSON).
	Signature    []byte         `json:"signature"`
	RulesVersion string         `json:"rules_version"`
	EventCounts  map[string]int `json:"event_counts"`
	SeqFrom      int64          `json:"seq_from"`
	SeqTo        int64          `json:"seq_to"`
	Buckets      int            `json:"buckets"`
	TimeFrom     *time.Time     `json:"time_from"`
	TimeTo       *time.Time     `json:"time_to"`
}

// Signed returns the record's signed fields for the device.
func (r EvidenceRecord) Signed(deviceID string) evidence.Day {
	utc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		u := t.UTC()
		return &u
	}
	return evidence.Day{
		DeviceID: deviceID, Day: r.Day, PrevChainHash: r.PrevChainHash, ContentHash: r.ContentHash,
		RulesVersion: r.RulesVersion, SeqFrom: r.SeqFrom, SeqTo: r.SeqTo, Buckets: r.Buckets,
		TimeFrom: utc(r.TimeFrom), TimeTo: utc(r.TimeTo),
	}
}

// Bundle is everything needed to check a submission without trusting the
// server (ADR 0020 "Checking it without us", ADR 0022): GET
// /api/admin/v1/submissions/{id}/bundle, read by logbook verify. Every
// part of it was frozen in the one snapshot taken at submission.
type Bundle struct {
	// Format is BundleFormat.
	Format     string           `json:"format"`
	Submission BundleSubmission `json:"submission"`
	// Devices are every device with a frozen row or a part.
	Devices []BundleDevice `json:"devices"`
	// Parts are the parts as uploaded, at most one per device.
	Parts []Part `json:"parts"`
	// Minutes are the frozen rows, every device.
	Minutes []Minute `json:"minutes"`
	// Capped are the frozen capped seconds of the project per minute.
	Capped []CappedMinute `json:"capped"`
	// CapInputs are every credited row of the user (any project, any
	// device) for the submitted minutes.
	CapInputs []CapInput `json:"cap_inputs"`
}

// BundleSubmission names the submission a bundle checks.
type BundleSubmission struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	CreatedAt time.Time `json:"created_at"`
}

// BundleDevice is a device's public key and its signed evidence records:
// every record with block fields (ADR 0019) the device has stored, oldest
// first, so the chain can be checked from its start.
type BundleDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// PublicKey is the device's 32-byte ed25519 key (standard base64).
	PublicKey    []byte           `json:"public_key"`
	EvidenceDays []EvidenceRecord `json:"evidence_days"`
}

// Range is a submission's inclusive range of UTC days.
type Range struct {
	From, To time.Time // midnight UTC of the first and last day
}

// ParseRange parses from and to (YYYY-MM-DD) into a range of at most
// MaxRangeDays days with from <= to.
func ParseRange(from, to string) (Range, error) {
	f, err := time.ParseInLocation(time.DateOnly, from, time.UTC)
	if err != nil {
		return Range{}, fmt.Errorf("%w: from %q is not a day", ErrMalformed, from)
	}
	t, err := time.ParseInLocation(time.DateOnly, to, time.UTC)
	if err != nil {
		return Range{}, fmt.Errorf("%w: to %q is not a day", ErrMalformed, to)
	}
	if t.Before(f) {
		return Range{}, fmt.Errorf("%w: to is before from", ErrMalformed)
	}
	if n := int(t.Sub(f)/(24*time.Hour)) + 1; n > MaxRangeDays {
		return Range{}, fmt.Errorf("%w: %d days, at most %d", ErrMalformed, n, MaxRangeDays)
	}
	return Range{From: f, To: t}, nil
}

// Days returns the range's days, YYYY-MM-DD, in order.
func (r Range) Days() []string {
	var out []string
	for d := r.From; !d.After(r.To); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(time.DateOnly))
	}
	return out
}

// Contains reports whether t is in one of the range's days.
func (r Range) Contains(t time.Time) bool {
	return !t.Before(r.From) && t.Before(r.To.AddDate(0, 0, 1))
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Validate checks an upload's shape: a project, a range (ParseRange) and a
// valid part. Whether the program, device and blocks exist is the
// server's to check.
func (u Upload) Validate() error {
	if u.Project == "" || len(u.Project) > 256 {
		return fmt.Errorf("%w: project", ErrMalformed)
	}
	if len(u.Program) > 64 {
		return fmt.Errorf("%w: program", ErrMalformed)
	}
	if _, err := ParseRange(u.From, u.To); err != nil {
		return err
	}
	return u.Part.Validate()
}

// Validate checks a part's shape: a lowercase UUID device id; blocks with
// distinct valid days and at least one bucket; and minutes that are
// distinct whole UTC minutes, each with a valid key whose window holds it
// ([T - 60 min, T), ADR 0017). Proofs are checked by VerifyPart.
func (p Part) Validate() error {
	if !uuidRe.MatchString(p.DeviceID) {
		return fmt.Errorf("%w: device_id %q", ErrMalformed, p.DeviceID)
	}
	days := make(map[string]bool, len(p.Blocks))
	for _, b := range p.Blocks {
		if _, err := time.Parse(time.DateOnly, b.Day); err != nil {
			return fmt.Errorf("%w: block day %q", ErrMalformed, b.Day)
		}
		if days[b.Day] {
			return fmt.Errorf("%w: two blocks for %s", ErrMalformed, b.Day)
		}
		days[b.Day] = true
		if b.Buckets < 1 {
			return fmt.Errorf("%w: block %s has no buckets", ErrMalformed, b.Day)
		}
	}
	seen := make(map[time.Time]bool, len(p.Minutes))
	for _, m := range p.Minutes {
		if err := m.validate(); err != nil {
			return err
		}
		if seen[m.Minute.UTC()] {
			return fmt.Errorf("%w: minute %s listed twice", ErrMalformed, m.Minute.UTC().Format(time.RFC3339))
		}
		seen[m.Minute.UTC()] = true
	}
	return nil
}

func (m PartMinute) validate() error {
	at := m.Minute.UTC().Format(time.RFC3339Nano)
	if !wholeMinute(m.Minute) || m.Minute.Unix() <= 0 {
		return fmt.Errorf("%w: minute %s", ErrMalformed, at)
	}
	if err := m.Key().Validate(); err != nil {
		return fmt.Errorf("%w: minute %s: %w", ErrMalformed, at, err)
	}
	if m.Minute.Before(m.ComputedAt.Add(-replay.Window)) || !m.Minute.Before(m.ComputedAt) {
		return fmt.Errorf("%w: minute %s is outside the window of its key", ErrMalformed, at)
	}
	return nil
}

func wholeMinute(t time.Time) bool { return t.Equal(t.Truncate(time.Minute)) }

// sortedCopy returns a sorted copy of s, never nil.
func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return out
}
