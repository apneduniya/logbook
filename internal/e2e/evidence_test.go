package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// The evidence chain of ADR 0019 end to end: blocks the agent seals are
// stored by the server with their signed block fields, and proofs the agent
// builds from its log and bucket lists are checked the way the server's
// re-check of a submission will (ADR 0020): against the records the server
// stored, with completeness of every window, and replayed with pkg/replay.

// serverDays returns the evidence days the server stores for the rig's
// device, oldest first, read as the re-check reads them. Each must carry
// block fields and verify with the device's key.
func (r *rig) serverDays() []ledger.StoredDay {
	r.t.Helper()
	days, err := ledger.NewService(r.st).Days(context.Background(), r.user, r.device,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, d := range days {
		if !d.HasBlock {
			r.t.Fatalf("the server stored %s without block fields", d.Record.Day)
		}
		if err := evidence.Verify(r.key.Public(), d.Record, d.ChainHash, d.Signature); err != nil {
			r.t.Fatalf("the server's record for %s: %v", d.Record.Day, err)
		}
	}
	return days
}

// dayText is a signed record as compared.
func dayText(d evidence.Day) string {
	t := func(p *time.Time) string {
		if p == nil {
			return "null"
		}
		return p.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s %s prev=%s content=%s rules=%s seq=(%d,%d] buckets=%d time=[%s,%s]", d.DeviceID, d.Day,
		d.PrevChainHash, d.ContentHash, d.RulesVersion, d.SeqFrom, d.SeqTo, d.Buckets, t(d.TimeFrom), t(d.TimeTo))
}

// serverMinute is the server's rows of one minute: their computation key
// and what a replay compares.
type serverMinute struct {
	key     replay.Key
	credits []replay.Credit
}

func (r *rig) serverMinute(at time.Time) serverMinute {
	r.t.Helper()
	rows := r.serverMinutes(at, at.Add(time.Minute))
	if len(rows) == 0 || rows[0].ComputedAt == nil || rows[0].Snapshot == nil {
		r.t.Fatalf("the server holds no keyed rows for %s: %+v", at.Format(time.DateTime), rows)
	}
	out := serverMinute{key: replay.Key{T: rows[0].ComputedAt.UTC(), H: *rows[0].Snapshot}}
	for _, s := range rows {
		out.credits = append(out.credits, replay.Credit{Project: s.Project, Label: s.Label, Credited: s.Credited, Cuts: s.Cuts})
	}
	return out
}

// serverMinuteTimes returns the minutes in [from, to) the server holds rows
// for.
func (r *rig) serverMinuteTimes(from, to time.Time) []time.Time {
	r.t.Helper()
	var out []time.Time
	for _, s := range r.serverMinutes(from, to) {
		if !slices.ContainsFunc(out, s.Minute.Equal) {
			out = append(out, s.Minute)
		}
	}
	return out
}

// proofsFor builds the agent's proofs for the keys of the server's rows of
// minutes, as one bundle, and fails if any key is unbacked.
func (r *rig) proofsFor(minutes []time.Time) crediting.Proofs {
	r.t.Helper()
	var keys []replay.Key
	for _, at := range minutes {
		if k := r.serverMinute(at).key; !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	p, err := crediting.BuildProofs(context.Background(), r.db, r.device.String(), keys)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(p.Unbacked) != 0 {
		r.t.Fatalf("unbacked keys %+v", p.Unbacked)
	}
	return p
}

// verifyProofs checks each proof of p against the record the server stored
// for its day (not the record the agent sent with it) and returns what each
// showed, by day.
func (r *rig) verifyProofs(p crediting.Proofs, stored []ledger.StoredDay) map[string]evidence.Verified {
	r.t.Helper()
	out := map[string]evidence.Verified{}
	for _, bp := range p.Blocks {
		i := slices.IndexFunc(stored, func(s ledger.StoredDay) bool { return s.Record.Day == bp.Record.Day })
		if i < 0 || stored[i].ChainHash != bp.ChainHash {
			r.t.Fatalf("a proof for %s, which the server doesn't hold as %s", bp.Record.Day, bp.ChainHash)
		}
		v, err := evidence.VerifyProof(bp.Proof, stored[i].Record)
		if err != nil {
			r.t.Fatalf("proof for %s: %v", bp.Record.Day, err)
		}
		out[bp.Record.Day] = v
	}
	return out
}

// bucketList returns the bucket list the agent stored for its block of day.
func (r *rig) bucketList(day string) []evidence.Bucket {
	r.t.Helper()
	ctx := context.Background()
	var list []evidence.Bucket
	if err := r.db.View(ctx, func(s *localdb.Snapshot) error {
		var err error
		list, err = s.Buckets(ctx, r.device.String(), day)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return list
}

// window is the minutes a run at k read: [T - 120 min, T).
func window(k replay.Key) evidence.MinuteRange {
	sp := k.Span()
	return evidence.MinuteRange{From: sp.History, To: sp.To}
}

// holder returns the stored block whose range holds h.
func holder(stored []ledger.StoredDay, h int64) (ledger.StoredDay, bool) {
	i := slices.IndexFunc(stored, func(s ledger.StoredDay) bool { return s.Record.SeqFrom < h && h <= s.Record.SeqTo })
	if i < 0 {
		return ledger.StoredDay{}, false
	}
	return stored[i], true
}

// revealed returns the rows a re-check replays key k from: every opened
// minute bucket, and the repo bucket of the block holding k.H only (ADR
// 0019 point 4).
func revealed(verified map[string]evidence.Verified, holderDay string) []evidence.Row {
	var rows []evidence.Row
	for day, v := range verified {
		for _, b := range v.Opened {
			if b.Key == evidence.RepoKey && day != holderDay {
				continue
			}
			rows = append(rows, b.Rows...)
		}
	}
	return rows
}

// recheckMinute does what the server's re-check does for the server's rows
// of minute at, with verified proofs: the block holding the key's H is
// stored and opens its repo bucket; every stored block that must show
// completeness for the key's window (evidence.NeedsCompleteness, from its
// signed fields) is proven complete; and pkg/replay at the key, over the
// revealed rows, reproduces the stored rows' label, project, credit and
// cuts. It returns how many blocks had to show completeness.
func (r *rig) recheckMinute(stored []ledger.StoredDay, verified map[string]evidence.Verified, at time.Time) int {
	r.t.Helper()
	sm := r.serverMinute(at)
	k, w := sm.key, window(sm.key)
	h, ok := holder(stored, k.H)
	if !ok {
		r.t.Fatalf("minute %s: no stored block holds H %d", at.Format(time.DateTime), k.H)
	}
	if v, ok := verified[h.Record.Day]; !ok || len(v.Opened) == 0 || v.Opened[0].I != 0 {
		r.t.Fatalf("minute %s: the proof for %s doesn't open its repo bucket", at.Format(time.DateTime), h.Record.Day)
	}
	needed := 0
	for _, s := range stored {
		if !evidence.NeedsCompleteness(s.Record, w, k.H) {
			continue
		}
		needed++
		v, ok := verified[s.Record.Day]
		if !ok {
			r.t.Fatalf("minute %s: no proof for %s, which may hold rows of its window", at.Format(time.DateTime), s.Record.Day)
		}
		if err := v.CheckComplete(w); err != nil {
			r.t.Fatalf("minute %s: %s: %v", at.Format(time.DateTime), s.Record.Day, err)
		}
	}
	res, err := replay.Run(revealed(verified, h.Record.Day), k, struggle.RulesVersion, time.UTC)
	if err != nil {
		r.t.Fatal(err)
	}
	if got := replay.Credits(res, at); !replay.Same(got, sm.credits) {
		r.t.Fatalf("minute %s at key %s/%d: server %+v, replay of the proofs %+v",
			at.Format(time.DateTime), k.T.Format(time.DateTime), k.H, sm.credits, got)
	}
	return needed
}

// firstRow returns the row of rows with the lowest sequence number.
func firstRow(rows []evidence.Row) evidence.Row {
	return slices.MinFunc(rows, func(a, b evidence.Row) int { return cmp.Compare(a.Seq, b.Seq) })
}

// busy logs the minute before now as the person working: activity samples,
// a save in app (and in lib on even minutes) and, every 5 minutes, an AI
// tool call.
func (r *rig) busy(now time.Time) {
	prev := now.Truncate(time.Minute).Add(-time.Minute)
	r.present(prev, prev.Add(time.Minute))
	projects := []string{"app"}
	if prev.Minute()%2 == 0 {
		projects = append(projects, "lib")
	}
	r.edit(prev.Add(20*time.Second), projects...)
	if prev.Minute()%5 == 0 {
		r.store(ai(prev.Add(30*time.Second), wire.AIToolStart))
	}
}

// A sealed day is stored with the block fields it was signed with, and the
// next day continues it: its prev_chain_hash is the first's chain hash and
// its seq_from the first's seq_to. Each block's fields match its rows, and
// the second opens with the repo snapshot logged when the first was frozen.
// A block that doesn't continue the chain's seq range is refused.
func TestEvidenceDaysChainBlockFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, m(-1).Add(5*time.Second))
	r.with(func(r *rig) { r.repos = []struggle.Repo{{Path: "/src/app", Project: "app"}} })
	r.runEvery(m(0), m(20), r.busy)
	r.finish("2026-11-20")
	nov21 := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	r.runEvery(nov21, nov21.Add(15*time.Minute), r.busy)
	r.finish("2026-11-21")

	days := r.serverDays()
	if len(days) != 2 {
		t.Fatalf("server days = %d, want 2", len(days))
	}
	first, second := days[0].Record, days[1].Record
	if first.SeqFrom != 0 || first.PrevChainHash != "" {
		t.Fatalf("first block = %s, want it to start the chain at 0", dayText(first))
	}
	if second.PrevChainHash != days[0].ChainHash || second.SeqFrom != first.SeqTo {
		t.Fatalf("second block = %s, want it to continue %s", dayText(second), dayText(first))
	}
	for _, s := range days {
		local, ok, err := r.db.EvidenceDayOf(ctx, r.device.String(), s.Record.Day)
		if err != nil || !ok {
			t.Fatalf("local %s: %v, %v", s.Record.Day, ok, err)
		}
		if got, want := dayText(s.Record), dayText(local.Record()); got != want {
			t.Fatalf("server stored\n  %s\nagent signed\n  %s", got, want)
		}
		rows, err := crediting.ReplayBlock(ctx, r.db, local)
		if err != nil {
			t.Fatal(err)
		}
		minutes := map[uint64]bool{}
		for _, row := range rows {
			if row.Seq <= s.Record.SeqFrom || row.Seq > s.Record.SeqTo {
				t.Fatalf("%s holds row %d outside %s", s.Record.Day, row.Seq, dayText(s.Record))
			}
			if k, err := evidence.BucketKey(row); err != nil {
				t.Fatal(err)
			} else if k != evidence.RepoKey {
				minutes[k] = true
			}
		}
		if int64(len(rows)) != s.Record.SeqTo-s.Record.SeqFrom {
			t.Fatalf("%s holds %d rows for %s", s.Record.Day, len(rows), dayText(s.Record))
		}
		if s.Record.Buckets != 1+len(minutes) || s.Record.TimeFrom == nil || s.Record.TimeTo == nil ||
			minutes[evidence.MinuteKey(s.Record.TimeFrom.Add(-time.Minute))] || !minutes[evidence.MinuteKey(*s.Record.TimeFrom)] ||
			!minutes[evidence.MinuteKey(*s.Record.TimeTo)] || minutes[evidence.MinuteKey(s.Record.TimeTo.Add(time.Minute))] {
			t.Fatalf("%s has %d minute buckets, signed %s", s.Record.Day, len(minutes), dayText(s.Record))
		}
		if s.Record.Day == second.Day {
			if f := firstRow(rows); f.Seq != second.SeqFrom+1 || f.Type != evidence.TypeRepo {
				t.Fatalf("the second block opens with row %d (%s), want the repo snapshot", f.Seq, f.Type)
			}
		}
	}

	// An identical re-upload is accepted; a third day overlapping the
	// second's range is refused even though it links and is signed.
	local, _, err := r.db.EvidenceDayOf(ctx, r.device.String(), second.Day)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.client.UploadEvidenceDay(ctx, r.target, r.device.String(), serverapi.EvidenceDayRequest{
		Day: second.Day, PrevChainHash: second.PrevChainHash, ContentHash: second.ContentHash, ChainHash: local.ChainHash,
		Signature: local.Signature, RulesVersion: second.RulesVersion, EventCounts: local.EventCounts,
		SeqFrom: second.SeqFrom, SeqTo: second.SeqTo, Buckets: second.Buckets, TimeFrom: second.TimeFrom, TimeTo: second.TimeTo,
	}); err != nil {
		t.Fatalf("identical re-upload: %v", err)
	}
	third := evidence.Day{DeviceID: r.device.String(), Day: "2026-11-22", PrevChainHash: days[1].ChainHash,
		ContentHash: strings.Repeat("cd", 32), RulesVersion: struggle.RulesVersion,
		SeqFrom: second.SeqTo - 1, SeqTo: second.SeqTo + 5, Buckets: 1}
	chain, sig, err := r.key.Sign(third)
	if err != nil {
		t.Fatal(err)
	}
	err = r.client.UploadEvidenceDay(ctx, r.target, third.DeviceID, serverapi.EvidenceDayRequest{
		Day: third.Day, PrevChainHash: third.PrevChainHash, ContentHash: third.ContentHash, ChainHash: chain,
		Signature: sig, RulesVersion: third.RulesVersion, EventCounts: map[string]int{},
		SeqFrom: third.SeqFrom, SeqTo: third.SeqTo, Buckets: third.Buckets,
	})
	var apiErr *serverapi.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != serverapi.KindConflict || !strings.Contains(apiErr.Message, "seq_from") {
		t.Fatalf("overlapping block: %v, want a seq conflict", err)
	}
}

// ADR 0019 point 4: once a block's bound is frozen, a row logged while it
// is still being hashed (here a late activity sample timed inside the
// day, in an hour the hash hasn't reached) is not in it, even though the
// cursor later reads that minute. It lands in the next block, whose signed
// span then starts at the late row's minute.
func TestLateRowMidHashLandsInNextBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, m(-60).Add(5*time.Second), withRunner(func(o *crediting.Options) { o.SealRows = 100 }))
	r.runEvery(m(-59), m(61), r.busy)

	for at := time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC); r.status().Evidence.Sealing != "2026-11-20"; at = at.Add(time.Minute) {
		if at.Hour() > 2 {
			t.Fatalf("the 20th isn't being sealed by %s: %+v", at.Format(time.DateTime), r.status())
		}
		r.set(at)
		r.run()
	}
	s, ok, err := r.db.Sealing(ctx)
	if err != nil || !ok || slices.Contains(r.sealedDays(), "2026-11-20") {
		t.Fatalf("sealing = %+v, %v, %v; want the 20th mid-hash", s, ok, err)
	}
	late := m(30).Add(7 * time.Second)
	if s.Cursor == evidence.RepoKey || s.Cursor > evidence.MinuteKey(late) {
		t.Fatalf("the hash is at %s, want it before %s", evidence.KeyMinute(s.Cursor).Format(time.DateTime), late.Format(time.DateTime))
	}
	r.store(sample(late, "editor"))
	lateSeq := r.high()
	if lateSeq <= s.SeqTo {
		t.Fatalf("late row %d is inside the frozen bound %d", lateSeq, s.SeqTo)
	}

	r.sealDay("2026-11-20")
	nov20, _, err := r.db.EvidenceDayOf(ctx, r.device.String(), "2026-11-20")
	if err != nil || nov20.SeqTo != s.SeqTo {
		t.Fatalf("the 20th = %+v, %v; want it to end at the frozen bound %d", nov20, err, s.SeqTo)
	}
	rows, err := crediting.ReplayBlock(ctx, r.db, nov20)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(rows, func(row evidence.Row) bool { return row.Seq == lateSeq }) {
		t.Fatal("the late row is in the block frozen before it was logged")
	}

	nov21 := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	r.runEvery(nov21, nov21.Add(10*time.Minute), r.busy)
	r.finish("2026-11-21")
	next, _, err := r.db.EvidenceDayOf(ctx, r.device.String(), "2026-11-21")
	if err != nil {
		t.Fatal(err)
	}
	rows, err = crediting.ReplayBlock(ctx, r.db, next)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(row evidence.Row) bool { return row.Seq == lateSeq && row.Time.Equal(late) }) {
		t.Fatalf("the late row %d isn't in the next block", lateSeq)
	}
	days := r.serverDays()
	if len(days) != 2 || days[1].Record.TimeFrom == nil || !days[1].Record.TimeFrom.Equal(m(30)) {
		t.Fatalf("server days = %+v, want the 21st's span to start at the late row's minute", days)
	}
}

// Work runs across midnight and on past the hour the 20th is sealed, so
// the windows of minutes after it span both blocks. For random minutes
// the agent builds one bundle of proofs; each verifies against the record
// the server stored, every window that needs it is proven complete, and
// pkg/replay at each minute's key reproduces the server's stored rows. A
// proof that hides one bucket of a window, or reveals it only as an edge,
// still verifies but fails the completeness check.
func TestProofsVerifyAgainstServerRecords(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC)
	end := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	r := newRig(t, start.Add(-time.Minute).Add(5*time.Second))
	r.with(func(r *rig) {
		r.repos = []struggle.Repo{{Path: "/src/app", Project: "app"}, {Path: "/src/lib", Project: "lib"}}
	})
	r.runEvery(start, end.Add(time.Minute), r.busy)
	r.runEvery(end.Add(time.Minute), end.Add(40*time.Minute), nil)
	r.finish("2026-11-21")
	stored := r.serverDays()
	if len(stored) != 2 {
		t.Fatalf("server days = %d, want 2", len(stored))
	}

	rng := rand.New(rand.NewPCG(20, 26))
	minutes := r.serverMinuteTimes(start, end)
	rng.Shuffle(len(minutes), func(i, j int) { minutes[i], minutes[j] = minutes[j], minutes[i] })
	minutes = minutes[:min(len(minutes), 40)]
	t.Run("random keys replay", func(t *testing.T) {
		verified := r.verifyProofs(r.proofsFor(minutes), stored)
		spanning := 0
		for _, at := range minutes {
			if r.recheckMinute(stored, verified, at) > 1 {
				spanning++
			}
		}
		if spanning == 0 {
			t.Fatal("no minute's window spans both blocks")
		}
		// One minute alone reveals less, and still replays.
		for _, at := range minutes[:5] {
			r.recheckMinute(stored, r.verifyProofs(r.proofsFor([]time.Time{at}), stored), at)
		}
	})

	t.Run("submit part verifies", func(t *testing.T) {
		r.wantSubmitPartVerifies(t, stored, start, end)
	})

	t.Run("hidden bucket fails completeness", func(t *testing.T) {
		at := minutes[0]
		k := r.serverMinute(at).key
		w := window(k)
		h, _ := holder(stored, k.H)
		p := r.proofsFor([]time.Time{at})
		i := slices.IndexFunc(p.Blocks, func(bp crediting.BlockProof) bool { return bp.Record.Day == h.Record.Day })
		if i < 0 {
			t.Fatalf("no proof for %s", h.Record.Day)
		}
		proof := p.Blocks[i].Proof
		v, err := evidence.VerifyProof(proof, h.Record)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.CheckComplete(w); err != nil {
			t.Fatalf("the honest proof: %v", err)
		}
		// Pick an opened minute bucket inside the window.
		j := slices.IndexFunc(v.Opened, func(b evidence.VerifiedBucket) bool {
			m := evidence.KeyMinute(b.Key)
			return b.Key != evidence.RepoKey && !m.Before(w.From) && m.Before(w.To)
		})
		if j < 0 {
			t.Fatalf("the proof for %s opens no minute of the window", h.Record.Day)
		}
		hidden := v.Opened[j].I
		list := r.bucketList(h.Record.Day)
		open := map[int][]evidence.Row{}
		for _, b := range v.Opened {
			if b.I != hidden {
				open[b.I] = b.Rows
			}
		}
		var edges []int
		for _, e := range proof.Edges {
			edges = append(edges, e.I)
		}
		for name, edges := range map[string][]int{"hidden": edges, "edge": append(slices.Clone(edges), hidden)} {
			slices.Sort(edges)
			cheat, err := evidence.BuildProof(list, open, edges)
			if err != nil {
				t.Fatal(err)
			}
			cv, err := evidence.VerifyProof(cheat, h.Record)
			if err != nil {
				t.Fatalf("%s: the proof itself should verify: %v", name, err)
			}
			if err := cv.CheckComplete(w); !errors.Is(err, evidence.ErrIncomplete) {
				t.Fatalf("%s: bucket %d (%s) left out, completeness = %v; want ErrIncomplete", name, hidden,
					evidence.KeyMinute(list[hidden].Key).Format(time.DateTime), err)
			}
		}
	})
}

// The repo mapping of /src/app changes twice inside one block: "zed",
// then "alpha", then "zed" again. Each minute is credited under the
// mapping at its key, and a re-check from the block's repo bucket (the
// highest seq per path at or below H) reproduces it. Handing the engine
// every mapping row instead (it breaks ties between equal paths by name)
// gets some minute wrong, so the reduction is what makes replay right.
func TestRepoMappingChangedMidBlockReplays(t *testing.T) {
	t.Parallel()
	r := newRig(t, m(-1).Add(5*time.Second))
	mapping := func(project string) {
		r.with(func(r *rig) { r.repos = []struggle.Repo{{Path: "/src/app", Project: project}} })
	}
	mapping("zed")
	r.runEvery(m(0), m(90), func(now time.Time) {
		switch now.Truncate(time.Minute) {
		case m(20):
			mapping("alpha")
		case m(40):
			mapping("zed")
		}
		if now.Before(m(61)) {
			r.busy(now)
		}
	})
	r.finish("2026-11-20")
	stored := r.serverDays()
	minutes := r.serverMinuteTimes(m(0), m(60))
	verified := r.verifyProofs(r.proofsFor(minutes), stored)
	projects := map[string]bool{}
	naiveWrong := 0
	for _, at := range minutes {
		r.recheckMinute(stored, verified, at)
		sm := r.serverMinute(at)
		for _, c := range sm.credits {
			projects[c.Project] = true
		}
		rows := revealed(verified, stored[0].Record.Day)
		var naive []struggle.Repo
		for _, row := range rows {
			if repo, ok := row.Value.(evidence.Repo); ok && row.Seq <= sm.key.H {
				naive = append(naive, struggle.Repo{Path: repo.Path, Project: repo.Project})
			}
		}
		in, err := replay.InputWithRepos(rows, naive, sm.key, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if !replay.Same(replay.Credits(struggle.Compute(in), at), sm.credits) {
			naiveWrong++
		}
	}
	if !projects["zed"] || !projects["alpha"] {
		t.Fatalf("projects credited = %v, want both mappings", projects)
	}
	if naiveWrong == 0 {
		t.Fatal("every minute replays the same without the per-path reduction; the test doesn't exercise it")
	}
}

// A device that replaces another (ADR 0017 point 5) starts its own chain
// at the log's high-water mark at the takeover: its first block's seq_from
// is that baseline and its first row the takeover's repo snapshot. The
// server accepts it as the device's first day, chains the next day to it,
// and the new device's minutes are backed by proofs that replay.
func TestTakeoverChainAccepted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, m(-1).Add(5*time.Second))
	r.with(func(r *rig) { r.repos = []struggle.Repo{{Path: "/src/app", Project: "app"}} })
	r.runEvery(m(0), m(20), r.busy)
	r.finish("2026-11-20")
	oldDevice := r.device
	old := r.serverDays()

	r.set(time.Date(2026, 11, 21, 5, 0, 5, 0, time.UTC))
	r.takeover()
	r.run()
	base, err := r.db.ChainBaseline(ctx, r.device.String())
	if err != nil || base <= old[0].Record.SeqTo {
		t.Fatalf("baseline = %d, %v; want it past the old device's last block (%d)", base, err, old[0].Record.SeqTo)
	}

	nov21 := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	r.runEvery(nov21, nov21.Add(45*time.Minute), func(now time.Time) {
		if now.Before(nov21.Add(21 * time.Minute)) {
			r.busy(now)
		}
	})
	r.sealDay("2026-11-21")
	nov22 := nov21.AddDate(0, 0, 1)
	r.runEvery(nov22, nov22.Add(40*time.Minute), func(now time.Time) {
		if now.Before(nov22.Add(11 * time.Minute)) {
			r.busy(now)
		}
	})
	r.sealDay("2026-11-22")

	days := r.serverDays()
	if len(days) != 2 || days[0].Record.SeqFrom != base || days[0].Record.PrevChainHash != "" ||
		days[1].Record.PrevChainHash != days[0].ChainHash || days[1].Record.SeqFrom != days[0].Record.SeqTo {
		t.Fatalf("new device's days = %v; want a chain from the baseline %d", days, base)
	}
	local, _, err := r.db.EvidenceDayOf(ctx, r.device.String(), "2026-11-21")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := crediting.ReplayBlock(ctx, r.db, local)
	if err != nil {
		t.Fatal(err)
	}
	if f := firstRow(rows); f.Seq != base+1 || f.Type != evidence.TypeRepo {
		t.Fatalf("the first block opens with row %d (%s), want the takeover's repo snapshot", f.Seq, f.Type)
	}
	oldNow, err := ledger.NewService(r.st).Days(ctx, r.user, oldDevice, m(0), nov22)
	if err != nil || len(oldNow) != 1 || oldNow[0].ChainHash != old[0].ChainHash {
		t.Fatalf("old device's days = %+v, %v; want them unchanged", oldNow, err)
	}

	var minutes []time.Time
	for _, d := range []time.Time{nov21, nov22} {
		minutes = append(minutes, r.serverMinuteTimes(d, d.Add(time.Hour))...)
	}
	if len(minutes) < 20 {
		t.Fatalf("only %d minutes stored for the new device", len(minutes))
	}
	verified := r.verifyProofs(r.proofsFor(minutes), days)
	for _, at := range minutes {
		r.recheckMinute(days, verified, at)
	}
}
