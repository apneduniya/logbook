package submission

import (
	"cmp"
	"crypto/ed25519"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// Result is what the re-check found for one frozen row.
type Result string

// Results of a frozen row (docs/api/submissions.md).
const (
	// ResultVerified: the server's replay agrees on label, project, credit
	// and cuts.
	ResultVerified Result = "verified"
	// ResultMismatch: the replay disagrees; the reviewer decides.
	ResultMismatch Result = "mismatch"
	// ResultUnbacked: no evidence shows what the run read.
	ResultUnbacked Result = "unbacked"
)

// Why a row is unbacked (RowResult.Reason).
const (
	ReasonNoPart     = "no evidence from this device yet"
	ReasonNoKey      = "stored before computation keys"
	ReasonNotListed  = "the device's part doesn't back it"
	ReasonNotSealed  = "its block isn't sealed"
	ReasonNoRepo     = "the repo bucket of its block isn't opened"
	ReasonIncomplete = "its window isn't proven complete"
	ReasonChainStart = "the device's evidence chain doesn't reach back to its start"
)

// ErrBadChain is wrapped when a device's signed records don't verify or
// don't form one chain.
var ErrBadChain = fmt.Errorf("%w: evidence chain", ErrMalformed)

// Credit is what a replay compares in a row: label, credit and cuts (the
// project is the row's).
type Credit struct {
	Label    string   `json:"label"`
	Credited int      `json:"credited"`
	Cuts     []string `json:"cuts"`
}

// RowResult is the re-check's result for one frozen row.
type RowResult struct {
	Row    Minute `json:"row"`
	Result Result `json:"result"`
	// Reason says why an unbacked row is unbacked (a Reason* constant).
	Reason string `json:"reason,omitempty"`
	// Server is the replay's row for the project on a mismatch: label
	// idle, no credit and no cuts when the replay has none.
	Server *Credit `json:"server,omitempty"`
}

// Device is what the verifier knows of one device: its public key and its
// signed evidence records. Records must be every record with block fields
// (ADR 0019) the device has stored, in any order: a run's window is only
// proven complete against all of them.
type Device struct {
	ID        string
	PublicKey ed25519.PublicKey
	Records   []EvidenceRecord
}

// Checked is what VerifyPart found.
type Checked struct {
	// Results has one entry per frozen row, in the order given.
	Results []RowResult
	// Revealed are every row the part opened, each checked against the
	// device's signed records, by sequence number. The review packet reads
	// them (BuildPacket).
	Revealed []evidence.Row
}

// Unbacked returns the result of rows that no part backs.
func Unbacked(rows []Minute, reason string) []RowResult {
	out := make([]RowResult, len(rows))
	for i, r := range rows {
		out[i] = RowResult{Row: r, Result: ResultUnbacked, Reason: reason}
	}
	return out
}

// VerifyPart re-checks one device's part against its signed records and
// the device's frozen rows (docs/api/submissions.md "The re-check job",
// ADR 0019 points 5 to 8):
//
//  1. The device's records verify with its key and form one chain: each
//     links to the previous by chain hash and starts at its seq_to. Every
//     block of the part names a record, and its proof verifies against it
//     (root, leaf keys, sequence bounds, every opened row in its right
//     bucket, sequences increasing).
//  2. Every minute the part lists names a frozen row of the device with
//     the same key.
//  3. A frozen row is unbacked unless it has a key (T, H), the part lists
//     it, a record holds H, the chain's first record is the device's first
//     ever (no previous chain hash) or starts at sequence 0, the part
//     opens the repo bucket of H's block, and every block whose signed
//     time span overlaps the run's window [T - 120 min, T) and whose
//     seq_from is below H proves completeness of the window.
//  4. Each backed row is replayed with pkg/replay at its key and rules
//     version, over the opened minute rows with seq <= H and the repo rows
//     of H's block reduced to the highest seq per path; rows sharing a key
//     and rules version share one run. Label, project, credit and cuts are
//     compared (flags are advisory): verified or mismatch.
//
// Any failure of 1 or 2, a frozen row of another device, or a rules
// version this build can't run is an error wrapping ErrMalformed: the
// submission fails rather than leaving minutes unbacked.
func VerifyPart(part Part, dev Device, frozen []Minute) (Checked, error) {
	if err := part.Validate(); err != nil {
		return Checked{}, err
	}
	if part.DeviceID != dev.ID {
		return Checked{}, fmt.Errorf("%w: part of device %s checked as %s", ErrMalformed, part.DeviceID, dev.ID)
	}
	chain, err := checkChain(dev)
	if err != nil {
		return Checked{}, err
	}
	blocks, err := verifyBlocks(part, dev.ID, chain)
	if err != nil {
		return Checked{}, err
	}
	if err := checkListed(part, dev.ID, frozen); err != nil {
		return Checked{}, err
	}
	ev := newEvidence(chain, blocks)

	listed := make(map[time.Time]bool, len(part.Minutes))
	for _, m := range part.Minutes {
		listed[m.Minute.UTC()] = true
	}
	out := Checked{Results: make([]RowResult, len(frozen)), Revealed: ev.revealed()}
	type run struct {
		key   replay.Key
		rules string
	}
	runs := map[run][]int{}
	backed := map[replay.Key]string{} // keys are UTC, so equal keys compare equal
	for i, row := range frozen {
		out.Results[i] = RowResult{Row: row}
		key, ok := row.Key()
		reason := ""
		switch {
		case !ok:
			reason = ReasonNoKey
		case !listed[row.Minute.UTC()]:
			reason = ReasonNotListed
		default:
			r, seen := backed[key]
			if !seen {
				r = ev.backs(key)
				backed[key] = r
			}
			reason = r
		}
		if reason != "" {
			out.Results[i].Result, out.Results[i].Reason = ResultUnbacked, reason
			continue
		}
		r := run{key: key, rules: row.RulesVersion}
		runs[r] = append(runs[r], i)
	}
	// Run in a fixed order, so an error names the same run every time.
	order := slices.SortedFunc(func(yield func(run) bool) {
		for r := range runs {
			if !yield(r) {
				return
			}
		}
	}, func(a, b run) int {
		return cmp.Or(a.key.T.Compare(b.key.T), cmp.Compare(a.key.H, b.key.H), cmp.Compare(a.rules, b.rules))
	})
	for _, r := range order {
		res, err := ev.replay(r.key, r.rules)
		if err != nil {
			return Checked{}, fmt.Errorf("%w: replaying %s at %d: %w", ErrMalformed,
				r.key.T.Format(time.RFC3339), r.key.H, err)
		}
		for _, i := range runs[r] {
			out.Results[i] = compare(frozen[i], res)
		}
	}
	return out, nil
}

// compare compares a frozen row with a replay's rows of its minute.
func compare(row Minute, res struggle.Result) RowResult {
	var server []replay.Credit
	for _, c := range replay.Credits(res, row.Minute) {
		if c.Project == row.Project {
			server = append(server, c)
		}
	}
	mine := replay.Credit{Project: row.Project, Label: row.Label, Credited: row.Credited, Cuts: row.Cuts}
	if replay.Same([]replay.Credit{mine}, server) {
		return RowResult{Row: row, Result: ResultVerified}
	}
	s := &Credit{Label: string(struggle.LabelIdle), Cuts: []string{}}
	if len(server) > 0 {
		s = &Credit{Label: server[0].Label, Credited: server[0].Credited, Cuts: sortedCopy(server[0].Cuts)}
	}
	return RowResult{Row: row, Result: ResultMismatch, Server: s}
}

// checkChain verifies every record of dev and that they form one chain,
// and returns them sorted by day.
func checkChain(dev Device) ([]EvidenceRecord, error) {
	if len(dev.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: device %s has no valid public key", ErrBadChain, dev.ID)
	}
	recs := slices.Clone(dev.Records)
	slices.SortFunc(recs, func(a, b EvidenceRecord) int { return cmp.Compare(a.Day, b.Day) })
	for i, r := range recs {
		if err := evidence.Verify(dev.PublicKey, r.Signed(dev.ID), r.ChainHash, r.Signature); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrBadChain, r.Day, err)
		}
		if i == 0 {
			continue
		}
		prev := recs[i-1]
		if r.Day == prev.Day {
			return nil, fmt.Errorf("%w: two records for %s", ErrBadChain, r.Day)
		}
		if r.PrevChainHash != prev.ChainHash || r.SeqFrom != prev.SeqTo {
			return nil, fmt.Errorf("%w: %s doesn't follow %s", ErrBadChain, r.Day, prev.Day)
		}
	}
	return recs, nil
}

// verifyBlocks verifies every block of part against its record.
func verifyBlocks(part Part, deviceID string, chain []EvidenceRecord) (map[string]evidence.Verified, error) {
	byDay := make(map[string]EvidenceRecord, len(chain))
	for _, r := range chain {
		byDay[r.Day] = r
	}
	out := make(map[string]evidence.Verified, len(part.Blocks))
	for _, b := range part.Blocks {
		rec, ok := byDay[b.Day]
		if !ok {
			return nil, fmt.Errorf("%w: block %s has no stored evidence day", ErrMalformed, b.Day)
		}
		v, err := evidence.VerifyProof(b.Proof, rec.Signed(deviceID))
		if err != nil {
			return nil, fmt.Errorf("%w: block %s: %w", ErrMalformed, b.Day, err)
		}
		out[b.Day] = v
	}
	return out, nil
}

// checkListed checks that frozen rows are the device's and that every
// minute the part lists is a frozen row of the device with the same key.
func checkListed(part Part, deviceID string, frozen []Minute) error {
	keys := make(map[time.Time][]replay.Key, len(frozen))
	for _, r := range frozen {
		if r.DeviceID != deviceID {
			return fmt.Errorf("%w: frozen row of device %s checked with %s", ErrMalformed, r.DeviceID, deviceID)
		}
		if k, ok := r.Key(); ok {
			keys[r.Minute.UTC()] = append(keys[r.Minute.UTC()], k)
		}
	}
	for _, m := range part.Minutes {
		ks := keys[m.Minute.UTC()]
		want := m.Key()
		if len(ks) == 0 || slices.ContainsFunc(ks, func(k replay.Key) bool { return !k.T.Equal(want.T) || k.H != want.H }) {
			return fmt.Errorf("%w: the part backs %s at (%s, %d), which isn't a frozen row of the device with that key",
				ErrMalformed, m.Minute.UTC().Format(time.RFC3339), want.T.Format(time.RFC3339), want.H)
		}
	}
	return nil
}

// partEvidence is a verified part: the device's chain, the verified blocks
// by day, and their opened rows.
type partEvidence struct {
	chain  []EvidenceRecord
	blocks map[string]evidence.Verified
	// events are the opened minute-bucket rows, by (time, seq).
	events []evidence.Row
	// repos are the opened repo bucket's rows of each block, by day.
	repos map[string][]evidence.Row
}

func newEvidence(chain []EvidenceRecord, blocks map[string]evidence.Verified) *partEvidence {
	e := &partEvidence{chain: chain, blocks: blocks, repos: map[string][]evidence.Row{}}
	for day, v := range blocks {
		for _, b := range v.Opened {
			if b.Key == evidence.RepoKey {
				e.repos[day] = b.Rows
				continue
			}
			e.events = append(e.events, b.Rows...)
		}
	}
	slices.SortFunc(e.events, func(a, b evidence.Row) int { return cmp.Or(a.Time.Compare(b.Time), cmp.Compare(a.Seq, b.Seq)) })
	return e
}

// revealed returns every opened row, by sequence number.
func (e *partEvidence) revealed() []evidence.Row {
	out := slices.Clone(e.events)
	for _, rows := range e.repos {
		out = append(out, rows...)
	}
	slices.SortFunc(out, func(a, b evidence.Row) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// holding returns the record whose block holds sequence number h.
func (e *partEvidence) holding(h int64) (EvidenceRecord, bool) {
	for _, r := range e.chain {
		if r.SeqFrom < h && h <= r.SeqTo {
			return r, true
		}
	}
	return EvidenceRecord{}, false
}

// window is the minutes a run at k read: [T - 120 min, T).
func window(k replay.Key) evidence.MinuteRange {
	s := k.Span()
	return evidence.MinuteRange{From: s.History, To: s.To}
}

// backs returns "" when the part backs a run at k, else why not.
func (e *partEvidence) backs(k replay.Key) string {
	hb, ok := e.holding(k.H)
	if !ok {
		return ReasonNotSealed
	}
	// Rows below the first record's seq_from are in no record. That is
	// sound only for the device's first record, which a takeover may
	// start above 0 (ADR 0019 point 4: the new device's windows start
	// after its registration); one after a day sealed before ADR 0019
	// starts again at 0. A first record that links to an earlier one and
	// starts above 0 (it follows a day stored without block fields, or
	// earlier records weren't given) leaves rows the run may have read
	// unproven.
	if first := e.chain[0]; first.PrevChainHash != "" && first.SeqFrom > 0 {
		return ReasonChainStart
	}
	v, ok := e.blocks[hb.Day]
	if !ok || !slices.ContainsFunc(v.Opened, func(b evidence.VerifiedBucket) bool { return b.Key == evidence.RepoKey }) {
		return ReasonNoRepo
	}
	w := window(k)
	for _, r := range e.chain {
		if !evidence.NeedsCompleteness(r.Signed(""), w, k.H) {
			continue
		}
		v, ok := e.blocks[r.Day]
		if !ok || v.CheckComplete(w) != nil {
			return ReasonIncomplete
		}
	}
	return ""
}

// replay runs the engine at k over the part's rows (backs(k) == "").
func (e *partEvidence) replay(k replay.Key, rules string) (struggle.Result, error) {
	compute, err := replay.Engine(rules)
	if err != nil {
		return struggle.Result{}, err
	}
	hb, _ := e.holding(k.H)
	w := window(k)
	lo := sort.Search(len(e.events), func(i int) bool { return !e.events[i].Time.Before(w.From) })
	hi := sort.Search(len(e.events), func(i int) bool { return !e.events[i].Time.Before(w.To) })
	in, err := replay.InputWithRepos(e.events[lo:hi], replay.Repos(e.repos[hb.Day], k.H), k, nil)
	if err != nil {
		return struggle.Result{}, err
	}
	return compute(in), nil
}
