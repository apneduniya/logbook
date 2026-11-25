package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/submit"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
)

// wantSubmitPartVerifies checks `logbook submit` end to end on the agent's
// side (ADR 0020): from the rows the server holds for project app in
// [from, to), the agent builds its part with crediting.BuildProofs, and
// the server's verifier, given the records the server stored and the
// device's key, verifies every minute. The summary reveals lib's rows in
// the windows and the repo folders. It runs inside a test that has
// already sealed the days (TestProofsVerifyAgainstServerRecords), so the
// rig's work isn't repeated.
func (r *rig) wantSubmitPartVerifies(t *testing.T, stored []ledger.StoredDay, from, to time.Time) {
	t.Helper()
	plan := submission.Plan{Project: "app", From: from.Format(time.DateOnly), To: to.Format(time.DateOnly)}
	for _, s := range r.serverMinutes(from, to) {
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
