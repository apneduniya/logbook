package submission

import (
	"cmp"
	"crypto/ed25519"
	"fmt"
	"slices"
	"time"
)

// Report is what CheckBundle found: the same counts the packet shows.
type Report struct {
	Minutes        Counts
	CountedSeconds int64
	// Results are every frozen row's result, by (device, minute).
	Results []RowResult
	// Revealed are the rows each part revealed, by device.
	Revealed []DeviceRows
}

// CheckBundle checks a verification bundle without trusting the server
// (ADR 0020 "Checking it without us"): every device's signed chain, every
// proof and every replay (VerifyPart; rows of a device with no part are
// unbacked), and the cap (CheckCap; the frozen capped minutes are exactly
// the frozen rows' minutes). It fails, wrapping ErrMalformed, when the
// bundle is inconsistent or any part is malformed.
func CheckBundle(b Bundle) (Report, error) {
	if b.Format != BundleFormat {
		return Report{}, fmt.Errorf("%w: format %q, want %q", ErrMalformed, b.Format, BundleFormat)
	}
	rg, err := ParseRange(b.Submission.From, b.Submission.To)
	if err != nil {
		return Report{}, err
	}
	devs := map[string]BundleDevice{}
	for _, d := range b.Devices {
		if _, dup := devs[d.ID]; dup {
			return Report{}, fmt.Errorf("%w: device %s listed twice", ErrMalformed, d.ID)
		}
		devs[d.ID] = d
	}
	rows := map[string][]Minute{}
	minutes := map[int64]bool{}
	for _, m := range b.Minutes {
		if _, ok := devs[m.DeviceID]; !ok {
			return Report{}, fmt.Errorf("%w: a row of unknown device %s", ErrMalformed, m.DeviceID)
		}
		if m.Project != b.Submission.Project || !rg.Contains(m.Minute) {
			return Report{}, fmt.Errorf("%w: row %s %q is outside the submission", ErrMalformed, m.Minute.UTC().Format(time.RFC3339), m.Project)
		}
		rows[m.DeviceID] = append(rows[m.DeviceID], m)
		minutes[m.Minute.Unix()] = true
	}
	parts := map[string]Part{}
	for _, p := range b.Parts {
		if _, ok := devs[p.DeviceID]; !ok {
			return Report{}, fmt.Errorf("%w: a part of unknown device %s", ErrMalformed, p.DeviceID)
		}
		if _, dup := parts[p.DeviceID]; dup {
			return Report{}, fmt.Errorf("%w: two parts of device %s", ErrMalformed, p.DeviceID)
		}
		parts[p.DeviceID] = p
	}
	var rep Report
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for id := range devs {
			if !yield(id) {
				return
			}
		}
	}) {
		dr := rows[id]
		slices.SortFunc(dr, func(a, b Minute) int { return cmp.Or(a.Minute.Compare(b.Minute), cmp.Compare(a.Project, b.Project)) })
		p, ok := parts[id]
		if !ok {
			rep.Results = append(rep.Results, Unbacked(dr, ReasonNoPart)...)
			continue
		}
		d := devs[id]
		c, err := VerifyPart(p, Device{ID: id, PublicKey: ed25519.PublicKey(d.PublicKey), Records: d.EvidenceDays}, dr)
		if err != nil {
			return Report{}, fmt.Errorf("device %s: %w", id, err)
		}
		rep.Results = append(rep.Results, c.Results...)
		rep.Revealed = append(rep.Revealed, DeviceRows{DeviceID: id, Rows: c.Revealed})
	}
	if err := CheckCap(b.Submission.Project, b.Minutes, b.Capped, b.CapInputs); err != nil {
		return Report{}, err
	}
	for _, c := range b.Capped {
		if !minutes[c.Minute.Unix()] {
			return Report{}, fmt.Errorf("%w: capped minute %s has no frozen row", ErrCapMismatch, c.Minute.UTC().Format(time.RFC3339))
		}
		rep.CountedSeconds += c.Seconds
	}
	rep.Minutes = CountMinutes(rep.Results)
	return rep, nil
}
