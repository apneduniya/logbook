package submissions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/submissions"
	"github.com/apneduniya/logbook/pkg/submission"
)

// The bundle built from the stored submission checks offline
// (submission.CheckBundle) to the counts the re-check stored; once the
// submission is withdrawn its evidence is gone and no bundle is built.
func TestBundleChecksOffline(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	created, _ := w.submit("terra")
	w.verify(created.ID)

	b, err := w.checker.Bundle(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Devices) != 2 || len(b.Parts) != 1 || b.Parts[0].DeviceID != w.a.id.String() ||
		b.Submission.ID != created.ID.String() || b.Submission.Project != "app" {
		t.Fatalf("bundle = %d devices, %d parts, %+v", len(b.Devices), len(b.Parts), b.Submission)
	}
	rep, err := submission.CheckBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	d := decode[detail](t, w.expect(call{target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice}, http.StatusOK))
	if rep.Minutes != d.Minutes || rep.CountedSeconds != d.CountedSeconds || rep.Minutes.Unbacked == 0 {
		t.Fatalf("offline %+v %d s, stored %+v %d s", rep.Minutes, rep.CountedSeconds, d.Minutes, d.CountedSeconds)
	}

	if _, err := w.checker.Bundle(ctx, uuid.New()); !errors.Is(err, submissions.ErrNotFound) {
		t.Fatalf("unknown submission: %v", err)
	}
	w.expect(call{method: http.MethodDelete, target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice},
		http.StatusNoContent)
	if _, err := w.checker.Bundle(ctx, created.ID); !errors.Is(err, submissions.ErrEvidenceDeleted) {
		t.Fatalf("withdrawn submission: %v", err)
	}
}

// WriteBundle streams the same JSON Bundle builds without decoding any
// part (so without a decode slot, even while every slot is taken), and
// says why it can't before writing anything.
func TestWriteBundleStreamsWithoutDecoding(t *testing.T) {
	t.Parallel()
	w := newWorld(t) // one decode slot
	ctx := context.Background()
	created, _ := w.submit("terra")
	w.verify(created.ID)

	decoded := 0
	submissions.SetDecodeHook(w.checker, func(delta int) {
		if delta > 0 {
			decoded++
		}
	})
	release, err := submissions.AcquireSlot(ctx, w.slots)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.checker.CheckBundle(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = w.checker.WriteBundle(ctx, created.ID, &out)
	release()
	if err != nil {
		t.Fatal(err)
	}
	if decoded != 0 {
		t.Fatalf("the export decoded %d parts", decoded)
	}
	var streamed submission.Bundle
	if err := json.Unmarshal(out.Bytes(), &streamed); err != nil {
		t.Fatalf("not one JSON value: %v\n%s", err, out.Bytes())
	}
	built, err := w.checker.Bundle(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(t, streamed), mustJSON(t, built); !bytes.Equal(a, b) {
		t.Fatalf("streamed:\n%s\nbuilt:\n%s", a, b)
	}
	if _, err := submission.CheckBundle(streamed); err != nil {
		t.Fatal(err)
	}

	if err := w.checker.CheckBundle(ctx, uuid.New()); !errors.Is(err, submissions.ErrNotFound) {
		t.Fatalf("unknown submission: %v", err)
	}
	w.expect(call{method: http.MethodDelete, target: "/api/v1/me/submissions/" + created.ID.String(), user: w.alice},
		http.StatusNoContent)
	if err := w.checker.CheckBundle(ctx, created.ID); !errors.Is(err, submissions.ErrEvidenceDeleted) {
		t.Fatalf("withdrawn: %v", err)
	}
	out.Reset()
	if err := w.checker.WriteBundle(ctx, created.ID, &out); !errors.Is(err, submissions.ErrEvidenceDeleted) || out.Len() != 0 {
		t.Fatalf("withdrawn: %v, wrote %d bytes", err, out.Len())
	}
}

// PartRows decodes a part only within a decode slot: while the only one
// is taken it waits, decoding nothing, until the request ends; then it
// pages through the opened rows and drops the part.
func TestPartRowsUsesTheDecodeSlots(t *testing.T) {
	t.Parallel()
	w := newWorld(t) // one decode slot
	ctx := context.Background()
	created, _ := w.submit("terra")
	w.verify(created.ID)

	release, err := submissions.AcquireSlot(ctx, w.slots)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, _, err = w.checker.PartRows(short, created.ID, w.a.id, 0, 10)
	cancel()
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with no slot free: %v", err)
	}

	held := 0
	submissions.SetDecodeHook(w.checker, func(delta int) { held += delta })
	all, total, err := w.checker.PartRows(ctx, created.ID, w.a.id, 0, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || len(all) != total || held != 0 {
		t.Fatalf("%d rows of %d, %d parts still held", len(all), total, held)
	}
	page, n, err := w.checker.PartRows(ctx, created.ID, w.a.id, 1, 2)
	if err != nil || n != total || len(page) != min(2, total-1) || (len(page) > 0 && page[0].Seq != all[1].Seq) {
		t.Fatalf("page = %+v of %d (%v)", page, n, err)
	}
	for _, r := range all {
		if r.Day == "" || r.Seq <= 0 || len(r.Value) == 0 || (r.Bucket != 0) != (r.BucketMinute != nil) {
			t.Fatalf("row %+v", r)
		}
	}
	if _, _, err := w.checker.PartRows(ctx, created.ID, w.b.id, 0, 10); !errors.Is(err, submissions.ErrNotFound) {
		t.Fatalf("a device without a part: %v", err)
	}
}
