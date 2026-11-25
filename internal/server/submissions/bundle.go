package submissions

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// ErrEvidenceDeleted means the submission's stored parts were deleted
// (withdrawal or retention), so no bundle can be built.
var ErrEvidenceDeleted = errors.New("the submission's evidence was deleted")

// bundleSource is everything in a submission's bundle but the parts
// themselves: those are read one at a time when they are written.
type bundleSource struct {
	head    submission.BundleSubmission
	devices []submission.BundleDevice
	// parts are the devices with a part, in device order.
	parts   []uuid.UUID
	minutes []submission.Minute
	capped  []submission.CappedMinute
	inputs  []submission.CapInput
}

// loadBundle reads a submission's bundle without its parts: the frozen
// rows, capped seconds and cap inputs, and every stored record with block
// fields and the public key of each device with a frozen row or a part.
// ErrNotFound for an unknown submission, ErrEvidenceDeleted once any of
// its parts is gone.
func (c *Checker) loadBundle(ctx context.Context, id uuid.UUID) (bundleSource, error) {
	sub, err := c.st.SubmissionGet(ctx, id)
	if err != nil {
		return bundleSource{}, notFound(err, ErrNotFound, "reading submission")
	}
	pr, err := c.st.SubmissionPartDevices(ctx, sub.ID)
	if err != nil {
		return bundleSource{}, fmt.Errorf("reading parts: %w", err)
	}
	frozen, err := c.st.SubmissionFrozenMinutes(ctx, sub.ID)
	if err != nil {
		return bundleSource{}, fmt.Errorf("reading frozen minutes: %w", err)
	}
	src := bundleSource{
		head: submission.BundleSubmission{
			ID: sub.ID.String(), Project: sub.Project, From: day(sub.FromDay), To: day(sub.ToDay), CreatedAt: sub.CreatedAt.UTC(),
		},
		devices: []submission.BundleDevice{},
		minutes: make([]submission.Minute, 0, len(frozen)),
	}
	ids := map[uuid.UUID]bool{}
	hasPart := map[uuid.UUID]bool{}
	for _, r := range pr {
		if r.Deleted {
			return bundleSource{}, ErrEvidenceDeleted
		}
		ids[r.DeviceID], hasPart[r.DeviceID] = true, true
	}
	for _, r := range frozen {
		src.minutes = append(src.minutes, minute(sub.Project, r.DeviceID, r.Minute, r.Label, r.Credited, r.Cuts, r.Flags,
			r.RulesVersion, r.ComputedAt, r.Snapshot.Int64, r.Snapshot.Valid))
		ids[r.DeviceID] = true
	}
	if src.capped, src.inputs, err = c.cap(ctx, sub.ID); err != nil {
		return bundleSource{}, err
	}
	devs, err := c.st.DeviceList(ctx, sub.UserID)
	if err != nil {
		return bundleSource{}, fmt.Errorf("reading devices: %w", err)
	}
	sorted := slices.SortedFunc(func(yield func(uuid.UUID) bool) {
		for id := range ids {
			if !yield(id) {
				return
			}
		}
	}, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, id := range sorted {
		i := slices.IndexFunc(devs, func(d db.Device) bool { return d.ID == id })
		if i < 0 {
			return bundleSource{}, fmt.Errorf("device %s of submission %s is gone", id, sub.ID)
		}
		records, _, err := c.records(ctx, sub.UserID, id)
		if err != nil {
			return bundleSource{}, err
		}
		if records == nil {
			records = []submission.EvidenceRecord{}
		}
		src.devices = append(src.devices, submission.BundleDevice{
			ID: id.String(), Name: devs[i].Name, PublicKey: devs[i].PublicKey, EvidenceDays: records,
		})
		if hasPart[id] {
			src.parts = append(src.parts, id)
		}
	}
	return src, nil
}

// Bundle builds a submission's verification bundle from what the server
// stored (docs/api/submissions.md "The verification bundle"): the frozen
// rows, capped seconds and cap inputs, the parts as stored, and every
// stored record with block fields and the public key of each device with
// a frozen row or a part. It holds every part decoded at once, so it is
// for tests and small submissions; the admin export streams the same JSON
// with WriteBundle. It checks no caller, so its callers must. ErrNotFound
// for an unknown submission, ErrEvidenceDeleted once its parts are gone.
func (c *Checker) Bundle(ctx context.Context, id uuid.UUID) (submission.Bundle, error) {
	src, err := c.loadBundle(ctx, id)
	if err != nil {
		return submission.Bundle{}, err
	}
	b := submission.Bundle{
		Format: submission.BundleFormat, Submission: src.head, Devices: src.devices, Parts: []submission.Part{},
		Minutes: src.minutes, Capped: src.capped, CapInputs: src.inputs,
	}
	for _, device := range src.parts {
		data, err := c.partData(ctx, id, device)
		if err != nil {
			return submission.Bundle{}, err
		}
		p, err := decodePart(data)
		if err != nil {
			return submission.Bundle{}, fmt.Errorf("part of device %s: %w", device, err)
		}
		b.Parts = append(b.Parts, p)
	}
	return b, nil
}

// partData reads one device's stored part, gzip-compressed:
// ErrEvidenceDeleted once its raw rows are gone, ErrNotFound when the
// device has no part.
func (c *Checker) partData(ctx context.Context, id, device uuid.UUID) ([]byte, error) {
	data, err := c.st.SubmissionPartData(ctx, db.SubmissionPartDataParams{SubmissionID: id, DeviceID: device})
	if store.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading the part of device %s: %w", device, err)
	}
	if data == nil {
		return nil, ErrEvidenceDeleted
	}
	return data, nil
}

// CheckBundle reports whether a submission's bundle can be exported: nil,
// ErrNotFound, or ErrEvidenceDeleted. Callers check before they start a
// response, since WriteBundle can't take back what it has written.
func (c *Checker) CheckBundle(ctx context.Context, id uuid.UUID) error {
	sub, err := c.st.SubmissionGet(ctx, id)
	if err != nil {
		return notFound(err, ErrNotFound, "reading submission")
	}
	deleted, err := c.st.SubmissionRawDeleted(ctx, sub.ID)
	if err != nil {
		return fmt.Errorf("reading parts: %w", err)
	}
	if deleted {
		return ErrEvidenceDeleted
	}
	return nil
}

// WriteBundle writes the JSON of the submission's bundle (Bundle's, as
// logbook verify reads it) to w without holding the parts in memory: it
// reads everything else first, then copies each stored part's JSON out of
// its gzip one part at a time, never decoding it. At most one stored part
// (its compressed bytes, at most what an upload may send, plus the
// decompressor's window) is held at once. Errors before the first byte
// are ErrNotFound or ErrEvidenceDeleted as for Bundle; an error after it
// means w holds a truncated bundle, which the caller must not pass off as
// whole.
func (c *Checker) WriteBundle(ctx context.Context, id uuid.UUID, w io.Writer) error {
	src, err := c.loadBundle(ctx, id)
	if err != nil {
		return err
	}
	bw := &jsonWriter{w: w}
	bw.raw(`{"format":`)
	bw.value(submission.BundleFormat)
	bw.raw(`,"submission":`)
	bw.value(src.head)
	bw.raw(`,"devices":`)
	writeArray(bw, src.devices)
	bw.raw(`,"parts":[`)
	for i, device := range src.parts {
		if bw.err != nil {
			break
		}
		if i > 0 {
			bw.raw(",")
		}
		bw.err = c.copyPart(ctx, id, device, bw)
	}
	bw.raw(`],"minutes":`)
	writeArray(bw, src.minutes)
	bw.raw(`,"capped":`)
	writeArray(bw, src.capped)
	bw.raw(`,"cap_inputs":`)
	writeArray(bw, src.inputs)
	bw.raw("}\n")
	return bw.err
}

// copyPart copies one stored part's JSON (as encodePart wrote it: compact,
// with a trailing newline, which JSON allows between array elements) to w.
// Reading to the end checks the gzip checksum.
func (c *Checker) copyPart(ctx context.Context, id, device uuid.UUID, w io.Writer) error {
	data, err := c.partData(ctx, id, device)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decompressing the part of device %s: %w", device, err)
	}
	defer func() { _ = zr.Close() }() // closing a gzip reader only releases it
	n, err := io.Copy(w, io.LimitReader(zr, maxStoredPartJSON+1))
	if err != nil {
		return fmt.Errorf("copying the part of device %s: %w", device, err)
	}
	if n > maxStoredPartJSON {
		return fmt.Errorf("the stored part of device %s is larger than any upload can make", device)
	}
	return nil
}

// maxStoredPartJSON bounds a stored part's JSON: encodePart re-encodes an
// upload of at most MaxDecompressedBytes, and escaping can make a byte at
// most six ("<" becomes "<").
const maxStoredPartJSON = 6*submission.MaxDecompressedBytes + 1<<20

// jsonWriter writes JSON pieces to w, keeping the first error.
type jsonWriter struct {
	w   io.Writer
	err error
}

func (j *jsonWriter) Write(p []byte) (int, error) {
	if j.err != nil {
		return 0, j.err
	}
	n, err := j.w.Write(p)
	j.err = err
	return n, err
}

func (j *jsonWriter) raw(s string) { _, _ = io.WriteString(j, s) } // the error is kept in j.err

func (j *jsonWriter) value(v any) {
	if j.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		j.err = fmt.Errorf("encoding bundle: %w", err)
		return
	}
	_, _ = j.Write(b) // the error is kept in j.err
}

// writeArray writes vs as a JSON array, one element at a time.
func writeArray[T any](j *jsonWriter, vs []T) {
	j.raw("[")
	for i, v := range vs {
		if i > 0 {
			j.raw(",")
		}
		j.value(v)
	}
	j.raw("]")
}
