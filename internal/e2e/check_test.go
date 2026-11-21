package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// The end-state checks of ADR 0017, run by every test.

// serverRow is one row of credited_minutes for the rig's device.
type serverRow struct {
	Minute         time.Time
	Project, Label string
	Credited       int
	Cuts, Flags    []string
	ComputedAt     *time.Time
	Snapshot       *int64
}

// serverMinutes returns the device's rows with from <= minute < to, by
// minute and project.
func (r *rig) serverMinutes(from, to time.Time) []serverRow {
	r.t.Helper()
	rows, err := r.st.Pool.Query(context.Background(),
		`SELECT minute, project, label, credited, cuts, flags, computed_at, snapshot FROM credited_minutes
		 WHERE device_id = $1 AND minute >= $2 AND minute < $3 ORDER BY minute, project`, r.device, from, to)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []serverRow
	for rows.Next() {
		var s serverRow
		if err := rows.Scan(&s.Minute, &s.Project, &s.Label, &s.Credited, &s.Cuts, &s.Flags, &s.ComputedAt, &s.Snapshot); err != nil {
			r.t.Fatal(err)
		}
		s.Minute = s.Minute.UTC()
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// serverContentHash is the content hash the server stored for day.
func (r *rig) serverContentHash(day string) string {
	r.t.Helper()
	var h string
	if err := r.st.Pool.QueryRow(context.Background(),
		`SELECT content_hash FROM evidence_days WHERE device_id = $1 AND day = $2::date`, r.device, day).Scan(&h); err != nil {
		r.t.Fatalf("server evidence for %s: %v", day, err)
	}
	return h
}

// rowText is a row as the server stores it, key included.
func rowText(at time.Time, project, label string, credited int, cuts, flags []string, key string) string {
	c, f := slices.Clone(cuts), slices.Clone(flags)
	slices.Sort(c)
	slices.Sort(f)
	return fmt.Sprintf("%s %q %s %d %v %v @%s", at.UTC().Format("01-02 15:04"), project, label, credited, c, f, key)
}

func localText(c localdb.CreditedMinute) string {
	key := "-"
	if !c.Key.IsZero() {
		key = fmt.Sprintf("%s/%d", c.Key.T.UTC().Format(time.RFC3339), c.Key.H)
	}
	return rowText(c.Minute, c.Project, c.Label, c.Credited, c.Cuts, c.Flags, key)
}

func (s serverRow) text() string {
	key := "-"
	if s.ComputedAt != nil && s.Snapshot != nil {
		key = fmt.Sprintf("%s/%d", s.ComputedAt.UTC().Format(time.RFC3339), *s.Snapshot)
	}
	return rowText(s.Minute, s.Project, s.Label, s.Credited, s.Cuts, s.Flags, key)
}

// creditText is what a re-run is compared on: project, label, credit, cuts.
func creditText(rows []localdb.CreditedMinute) string {
	out := make([]string, 0, len(rows))
	for _, c := range rows {
		cuts := slices.Clone(c.Cuts)
		slices.Sort(cuts)
		out = append(out, fmt.Sprintf("[%q %s %d %v]", c.Project, c.Label, c.Credited, cuts))
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// captureBlocks reads back, from the local log, the rows of every block
// the server has accepted since the last call, and checks they hash to the
// content hash the agent signed and the server stored. A legacy day (sealed
// before the event log) has no rows to read.
func (r *rig) captureBlocks() {
	r.t.Helper()
	ctx := context.Background()
	for _, day := range r.sealedDays() {
		if _, ok := r.bundle[day]; ok {
			continue
		}
		e, ok, err := r.db.EvidenceDayOf(ctx, r.device.String(), day)
		if err != nil || !ok {
			r.t.Fatalf("the server holds evidence for %s, the agent has none (%v)", day, err)
		}
		if e.Legacy {
			r.bundle[day] = nil
			continue
		}
		rows := r.replayBlock(e)
		r.bundle[day] = rows
	}
}

// replayBlock replays e from the local log and checks its hash.
func (r *rig) replayBlock(e localdb.EvidenceDay) []evidence.Row {
	r.t.Helper()
	rows, err := crediting.ReplayBlock(context.Background(), r.db, e)
	if err != nil {
		r.t.Fatalf("replaying block %s: %v", e.Day, err)
	}
	h, err := evidence.ContentHash(rows)
	if err != nil {
		r.t.Fatal(err)
	}
	if h != e.ContentHash || h != r.serverContentHash(e.Day) {
		r.t.Fatalf("block %s re-hashes to %s; signed %s, server %s", e.Day, h, e.ContentHash, r.serverContentHash(e.Day))
	}
	return rows
}

// finish runs the agent, with both clocks right and the server up, from
// the end of day plus the grace hour (or now, if later) until day is
// sealed, then checks the end state.
func (r *rig) finish(day string) checked {
	r.t.Helper()
	r.with(func(r *rig) { r.serverOffset, r.down, r.date = 0, false, "" })
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		r.t.Fatal(err)
	}
	at := d.AddDate(0, 0, 1).Add(crediting.DayGrace)
	if now := r.clock().Truncate(time.Minute); !now.Before(at) {
		at = now.Add(time.Minute)
	}
	for deadline := at.Add(40 * time.Minute); !slices.Contains(r.sealedDays(), day); at = at.Add(time.Minute) {
		if at.After(deadline) {
			r.t.Fatalf("%s not sealed by %s: status %+v", day, at.Format(time.DateTime), r.status())
		}
		r.set(at.Add(5 * time.Second))
		r.run()
	}
	return r.check()
}

// checked counts what check verified.
type checked struct {
	// minutes is how many stored minutes were compared; unkeyed how many
	// of them hold rows adopted from the server without a key; fromBlocks
	// how many were replayed from the sealed blocks alone.
	minutes, unkeyed, fromBlocks int
}

// check verifies the end state ADR 0017 promises:
//
//   - every stored minute is final and acknowledged, and the server holds
//     exactly the agent's rows, keys included, and no others;
//   - each minute's rows share one key, and a replay at that key
//     (crediting.ReplayMinute) reproduces their label, project, credit and
//     cuts, both from the local log (unless the test pruned it) and from a
//     log rebuilt from the sealed blocks alone, as a step-5 reviewer would,
//     for every minute of a day the chain has moved past;
//   - such a minute's key is at or below the end of the blocks sealed up to
//     its day, and every sealed block re-hashes to the content hash the
//     agent signed and the server stored.
//
// A minute adopted from the server without a key must be refused for
// replay (ErrNotReproducible); check counts them.
func (r *rig) check() checked {
	r.t.Helper()
	ctx := context.Background()
	from, to := time.Unix(0, 0), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	local, err := r.db.StoredMinutes(ctx, from, to)
	if err != nil {
		r.t.Fatal(err)
	}
	server := r.serverMinutes(from, to)
	byMinute := map[time.Time][]localdb.CreditedMinute{}
	var localTexts, serverTexts []string
	for _, c := range local {
		if !c.Final || !c.Acknowledged {
			r.t.Fatalf("minute %s %q is not final and acknowledged at the end: %+v", c.Minute.Format(time.DateTime), c.Project, c)
		}
		if isRemoval(c) {
			continue // the server holds no row for it
		}
		byMinute[c.Minute] = append(byMinute[c.Minute], c)
		localTexts = append(localTexts, localText(c))
	}
	for _, s := range server {
		serverTexts = append(serverTexts, s.text())
	}
	if !slices.Equal(localTexts, serverTexts) {
		r.t.Fatalf("the agent's final minutes differ from the server's:\nagent  %s\nserver %s",
			strings.Join(localTexts, "\n       "), strings.Join(serverTexts, "\n       "))
	}

	blocks := map[string]localdb.EvidenceDay{}
	for _, day := range r.sealedDays() {
		e, ok, err := r.db.EvidenceDayOf(ctx, r.device.String(), day)
		if err != nil || !ok || e.State != localdb.EvidenceAccepted {
			r.t.Fatalf("evidence for %s = %+v, %v, %v", day, e, ok, err)
		}
		if e.Legacy {
			if _, err := crediting.ReplayBlock(ctx, r.db, e); !errors.Is(err, crediting.ErrNotReproducible) {
				r.t.Fatalf("replaying legacy day %s: %v, want ErrNotReproducible", day, err)
			}
			continue
		}
		blocks[day] = e
		if !r.pruned {
			r.replayBlock(e) // still re-hashes at the end
		}
	}
	rebuilt := r.rebuild()
	through := r.sealedThrough()

	var res checked
	for _, at := range slices.SortedFunc(maps.Keys(byMinute), time.Time.Compare) {
		rows := byMinute[at]
		res.minutes++
		key := rows[0].Key
		for _, c := range rows {
			if c.Key != key {
				r.t.Fatalf("minute %s mixes keys: %+v", at.Format(time.DateTime), rows)
			}
		}
		if key.IsZero() {
			res.unkeyed++
			if _, err := crediting.ReplayMinute(ctx, r.db, at, key, struggle.Compute, time.UTC); !errors.Is(err, crediting.ErrNotReproducible) {
				r.t.Fatalf("replaying unkeyed minute %s: %v, want ErrNotReproducible", at.Format(time.DateTime), err)
			}
			continue
		}
		want := creditText(rows)
		if !r.pruned {
			r.wantReplay(r.db, "the local log", at, key, want)
		}
		if !at.Before(through) {
			continue // its day is not closed yet
		}
		// The chain has moved past this minute's day: sealed, or skipped
		// because no row was logged since the previous block. Either way
		// the blocks up to that day hold every row of the minute's key.
		end := int64(0)
		for day, e := range blocks {
			if day <= at.UTC().Format(time.DateOnly) {
				end = max(end, e.SeqTo)
			}
		}
		if key.H > end {
			r.t.Fatalf("minute %s has key H %d past the sealed blocks' end %d", at.Format(time.DateTime), key.H, end)
		}
		r.wantReplay(rebuilt, "the sealed blocks", at, key, want)
		res.fromBlocks++
	}
	r.t.Logf("checked %d minutes (%d unkeyed, %d replayed from %d sealed blocks)", res.minutes, res.unkeyed, res.fromBlocks, len(blocks))
	return res
}

// sealedThrough is where the agent's evidence chain stands: the end of the
// latest day it sealed or skipped (zero if none).
func (r *rig) sealedThrough() time.Time {
	r.t.Helper()
	v, err := r.db.Get(context.Background(), "crediting.sealed_through")
	if err != nil {
		r.t.Fatal(err)
	}
	if v == "" {
		return time.Time{}
	}
	_, end, err := evidence.DayBounds(v) // the latest day, YYYY-MM-DD
	if err != nil {
		r.t.Fatal(err)
	}
	return end
}

// wantReplay checks that minute at, replayed from log at key, gives want.
func (r *rig) wantReplay(log crediting.LogReader, from string, at time.Time, key localdb.Key, want string) {
	r.t.Helper()
	replayed, err := crediting.ReplayMinute(context.Background(), log, at, key, struggle.Compute, time.UTC)
	if err != nil {
		r.t.Fatalf("replaying %s from %s: %v", at.Format(time.DateTime), from, err)
	}
	if got := creditText(replayed); got != want {
		r.t.Fatalf("minute %s at key %s/%d: stored %s, replay from %s %s",
			at.Format(time.DateTime), key.T.Format("15:04"), key.H, want, from, got)
	}
}

// rebuild builds a new local log from the sealed blocks alone (the rows
// captured when each was accepted), inserting the rows in sequence order
// the way the agent logs them, and checks every row gets back its own
// sequence number. It returns nil when nothing is sealed. A replay from it
// uses nothing but what the evidence chain signed.
func (r *rig) rebuild() *localdb.DB {
	r.t.Helper()
	var rows []evidence.Row
	for _, b := range r.bundle {
		rows = append(rows, b...)
	}
	if len(rows) == 0 {
		return nil
	}
	slices.SortFunc(rows, func(a, b evidence.Row) int { return cmp.Compare(a.Seq, b.Seq) })
	if rows[0].Seq != 1 {
		r.t.Fatalf("the sealed blocks start at row %d, not the log's first", rows[0].Seq)
	}
	ctx := context.Background()
	d := r.openDB(filepath.Join(r.t.TempDir(), "rebuilt.db"))
	for i := 0; i < len(rows); {
		j := i + 1
		for j < len(rows) && rows[j].Type == rows[i].Type && rows[i].Type != evidence.TypeRepo {
			j++
		}
		var err error
		switch group := rows[i:j]; rows[i].Type {
		case evidence.TypeHeartbeat:
			hbs := make([]wire.Heartbeat, len(group))
			for k, row := range group {
				hbs[k] = row.Value.(wire.Heartbeat)
			}
			err = d.Enqueue(ctx, hbs, false)
		case evidence.TypeRepo:
			repo := group[0].Value.(evidence.Repo)
			err = d.RecordRepos(ctx, group[0].Time, []struggle.Repo{{Path: repo.Path, Project: repo.Project}})
		case evidence.TypeActivity, evidence.TypeAI, evidence.TypeGit:
			evs := make([]wire.LocalEvent, len(group))
			for k, row := range group {
				switch v := row.Value.(type) {
				case wire.ActivitySample:
					evs[k] = wire.LocalEvent{Type: "activity", Activity: &v}
				case wire.AIEvent:
					evs[k] = wire.LocalEvent{Type: "ai", AI: &v}
				case wire.GitEvent:
					evs[k] = wire.LocalEvent{Type: "git", Git: &v}
				}
			}
			err = d.InsertEvents(ctx, evs)
		default:
			r.t.Fatalf("row %d: the e2e rebuild doesn't log %s rows", rows[i].Seq, rows[i].Type)
		}
		if err != nil {
			r.t.Fatal(err)
		}
		w, err := d.ReadWindow(ctx, time.Time{}, time.Time{}, 0)
		if err != nil {
			r.t.Fatal(err)
		}
		if w.H != rows[j-1].Seq {
			r.t.Fatalf("rebuilding the log: rows up to %d took numbers up to %d", rows[j-1].Seq, w.H)
		}
		i = j
	}
	return d
}
