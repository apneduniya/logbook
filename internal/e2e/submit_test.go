package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/submit"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
)

// `logbook submit` end to end on the agent's side (ADR 0020): from the
// rows the server holds for project app over two days, the agent builds
// its part with crediting.BuildProofs, and the server's verifier, given
// the records the server stored and the device's key, verifies every
// minute. The summary reveals lib's rows in the windows and the repo
// folders.
func TestSubmitPartVerifiesAgainstServer(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC)
	end := time.Date(2026, 11, 21, 1, 30, 0, 0, time.UTC)
	r := newRig(t, start.Add(-time.Minute).Add(5*time.Second))
	r.with(func(r *rig) {
		r.repos = []struggle.Repo{{Path: "/src/app", Project: "app"}, {Path: "/src/lib", Project: "lib"}}
	})
	r.runEvery(start, end.Add(time.Minute), r.busy)
	r.runEvery(end.Add(time.Minute), end.Add(40*time.Minute), nil)
	r.finish("2026-11-21")
	stored := r.serverDays()

	plan := submission.Plan{Project: "app", From: "2026-11-20", To: "2026-11-21"}
	for _, s := range r.serverMinutes(start.Add(-time.Hour), end.Add(time.Hour)) {
		if s.Project != "app" {
			continue
		}
		plan.Minutes = append(plan.Minutes, submission.Minute{DeviceID: r.device.String(), Minute: s.Minute, Project: s.Project,
			Label: s.Label, Credited: s.Credited, Cuts: s.Cuts, Flags: s.Flags, RulesVersion: struggle.RulesVersion,
			ComputedAt: s.ComputedAt, Snapshot: s.Snapshot})
	}
	if len(plan.Minutes) < 100 {
		t.Fatalf("the server holds %d rows of app", len(plan.Minutes))
	}
	keys := submit.Keys(plan, r.device.String())
	proofs, err := crediting.BuildProofs(context.Background(), r.db, r.device.String(), keys)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := submit.Prepare(plan, r.device.String(), proofs)
	if err != nil {
		t.Fatal(err)
	}
	if len(prep.Part.Minutes) != len(plan.Minutes) || len(prep.Summary.Unbacked) != 0 || len(prep.Part.Blocks) != 2 {
		t.Fatalf("part lists %d of %d minutes in %d blocks; unbacked %+v", len(prep.Part.Minutes), len(plan.Minutes),
			len(prep.Part.Blocks), prep.Summary.Unbacked)
	}
	var recs []submission.EvidenceRecord
	for _, d := range stored {
		recs = append(recs, submission.EvidenceRecord{Day: d.Record.Day, PrevChainHash: d.Record.PrevChainHash,
			ContentHash: d.Record.ContentHash, ChainHash: d.ChainHash, Signature: d.Signature, RulesVersion: d.Record.RulesVersion,
			EventCounts: d.EventCounts, SeqFrom: d.Record.SeqFrom, SeqTo: d.Record.SeqTo, Buckets: d.Record.Buckets,
			TimeFrom: d.Record.TimeFrom, TimeTo: d.Record.TimeTo})
	}
	c, err := submission.VerifyPart(prep.Part, submission.Device{ID: r.device.String(), PublicKey: r.key.Public(), Records: recs}, plan.Minutes)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range c.Results {
		if res.Result != submission.ResultVerified {
			t.Fatalf("%s: %s %s %+v", res.Row.Minute.Format(time.DateTime), res.Result, res.Reason, res.Server)
		}
	}

	rv := prep.Summary.Revealed
	if rv.Project.Heartbeats == 0 || len(rv.Others) != 1 || rv.Others[0].Project != "lib" || rv.Others[0].Heartbeats == 0 ||
		len(rv.Repos) != 2 || rv.Activity == 0 {
		t.Fatalf("revealed = %+v", rv)
	}
}
