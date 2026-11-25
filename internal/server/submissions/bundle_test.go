package submissions_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

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
