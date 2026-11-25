package audit

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/apneduniya/logbook/pkg/submission"
)

// MaxBundleBytes bounds a bundle once decompressed. A bundle holds every
// part of a submission (each at most submission.MaxDecompressedBytes) and
// the devices' records, so it is larger than one upload.
const MaxBundleBytes = 1 << 30

// MaxMismatches is how many mismatched rows a report lists.
const MaxMismatches = 20

// ErrTooLarge means the bundle is over MaxBundleBytes decompressed.
var ErrTooLarge = errors.New("the bundle is too large")

// Read decodes a bundle from r, which holds its JSON, gzip-compressed or
// not (told apart by the gzip header, not the file name).
func Read(r io.Reader) (submission.Bundle, error) { return read(r, MaxBundleBytes) }

// read is Read with a limit of n decompressed bytes.
func read(r io.Reader, n int64) (submission.Bundle, error) {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return submission.Bundle{}, fmt.Errorf("reading the bundle: %w", err)
		}
		defer func() { _ = zr.Close() }() // only releases the reader; the data is read below
		src = zr
	}
	var b submission.Bundle
	if err := json.NewDecoder(&limited{r: src, n: n}).Decode(&b); err != nil {
		if errors.Is(err, ErrTooLarge) {
			return submission.Bundle{}, err
		}
		return submission.Bundle{}, fmt.Errorf("reading the bundle: %w", err)
	}
	return b, nil
}

// limited reads at most n bytes of r, then fails with ErrTooLarge.
type limited struct {
	r io.Reader
	n int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// Report is what Check found.
type Report struct {
	Submission submission.BundleSubmission
	// Devices are every device of the bundle, by id.
	Devices []DeviceReport
	// Cap is the cap check's failure, nil when it passed; CapMinutes and
	// CapInputs say what it checked.
	Cap                   error
	CapMinutes, CapInputs int
	// Err is submission.CheckBundle's failure, nil when the whole bundle
	// checks. It decides the result; the steps above say where it failed.
	Err error
	// The counts the review packet shows, set when Err is nil.
	Minutes        submission.Counts
	CountedSeconds int64
	// Unbacked counts unbacked rows by reason, and Mismatches lists the
	// first MaxMismatches mismatched rows of MismatchRows.
	Unbacked     map[string]int
	Mismatches   []submission.RowResult
	MismatchRows int
}

// DeviceReport is one device's checks.
type DeviceReport struct {
	ID, Name string
	// Records is how many signed records the bundle holds for it, and
	// Chain why they don't verify or form one chain (nil when they do).
	Records int
	Chain   error
	// HasPart is true when the device uploaded a part: Blocks and Listed
	// count its blocks and listed minutes, and Proofs is why it doesn't
	// verify (nil when it does). Rows counts the device's frozen rows.
	HasPart        bool
	Blocks, Listed int
	Proofs         error
	Rows           int
}

// OK reports whether every check passed.
func (r Report) OK() bool {
	if r.Err != nil || r.Cap != nil {
		return false
	}
	return !slices.ContainsFunc(r.Devices, func(d DeviceReport) bool { return d.Chain != nil || d.Proofs != nil })
}

// Check checks b step by step, then as a whole with
// submission.CheckBundle, whose answer decides.
func Check(b submission.Bundle) Report {
	r := Report{Submission: b.Submission, CapMinutes: len(b.Capped), CapInputs: len(b.CapInputs)}
	rows := map[string][]submission.Minute{}
	for _, m := range b.Minutes {
		rows[m.DeviceID] = append(rows[m.DeviceID], m)
	}
	parts := map[string]submission.Part{}
	for _, p := range b.Parts {
		if _, dup := parts[p.DeviceID]; !dup {
			parts[p.DeviceID] = p
		}
	}
	devs := slices.Clone(b.Devices)
	slices.SortStableFunc(devs, func(x, y submission.BundleDevice) int { return cmp.Compare(x.ID, y.ID) })
	for _, d := range devs {
		r.Devices = append(r.Devices, checkDevice(d, parts, rows[d.ID]))
	}
	r.Cap = submission.CheckCap(b.Submission.Project, b.Minutes, b.Capped, b.CapInputs)

	rep, err := submission.CheckBundle(b)
	if err != nil {
		r.Err = err
		return r
	}
	r.Minutes, r.CountedSeconds = rep.Minutes, rep.CountedSeconds
	r.Unbacked = map[string]int{}
	for _, res := range rep.Results {
		switch res.Result {
		case submission.ResultUnbacked:
			r.Unbacked[res.Reason]++
		case submission.ResultMismatch:
			r.MismatchRows++
			if len(r.Mismatches) < MaxMismatches {
				r.Mismatches = append(r.Mismatches, res)
			}
		}
	}
	return r
}

// checkDevice verifies a device's chain and, when it has one, its part.
// VerifyPart checks the chain first, so a part's failure that isn't
// submission.ErrBadChain is a failure of its proofs or replays.
func checkDevice(d submission.BundleDevice, parts map[string]submission.Part, rows []submission.Minute) DeviceReport {
	out := DeviceReport{ID: d.ID, Name: d.Name, Records: len(d.EvidenceDays), Rows: len(rows)}
	dev := submission.Device{ID: d.ID, PublicKey: ed25519.PublicKey(d.PublicKey), Records: d.EvidenceDays}
	p, ok := parts[d.ID]
	if !ok {
		// An empty part checks only the chain.
		if _, err := submission.VerifyPart(submission.Part{DeviceID: d.ID}, dev, nil); err != nil {
			out.Chain = err
		}
		return out
	}
	out.HasPart, out.Blocks, out.Listed = true, len(p.Blocks), len(p.Minutes)
	slices.SortFunc(rows, func(x, y submission.Minute) int {
		return cmp.Or(x.Minute.Compare(y.Minute), cmp.Compare(x.Project, y.Project))
	})
	_, err := submission.VerifyPart(p, dev, rows)
	switch {
	case errors.Is(err, submission.ErrBadChain):
		out.Chain = err
	case err != nil:
		out.Proofs = err
	}
	return out
}

// reasons returns the unbacked reasons, most rows first.
func (r Report) reasons() []string {
	return slices.SortedFunc(maps.Keys(r.Unbacked), func(a, b string) int {
		return cmp.Or(cmp.Compare(r.Unbacked[b], r.Unbacked[a]), cmp.Compare(a, b))
	})
}

func (r Report) created() string {
	if r.Submission.CreatedAt.IsZero() {
		return "unknown"
	}
	return r.Submission.CreatedAt.UTC().Format(time.RFC3339)
}
