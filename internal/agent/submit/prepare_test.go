package submit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"reflect"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/submission"
)

func prepared(t *testing.T, f *fixture) Prepared {
	t.Helper()
	proofs, err := f.prove(context.Background(), Keys(f.plan, laptop))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(f.plan, laptop, proofs)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Keys asks for the laptop's keyed minutes only: not the desktop's, and
// not the row stored before keys.
func TestKeys(t *testing.T) {
	f := newFixture(t)
	got := Keys(f.plan, laptop)
	want := []replay.Key{f.key, f.unprovable}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Keys = %+v, want %+v", got, want)
	}
}

// The part the agent builds is what the server's verifier accepts: every
// listed minute verifies against the signed record, and the minutes it
// can't back are unbacked, never failed.
func TestPreparedPartVerifies(t *testing.T) {
	f := newFixture(t)
	p := prepared(t, f)
	if len(p.Part.Blocks) != 1 || p.Part.Blocks[0].Day != "2026-11-20" {
		t.Fatalf("blocks = %+v", p.Part.Blocks)
	}
	own := f.own()
	if len(p.Part.Minutes) != len(own)-2 {
		t.Fatalf("listed %d minutes of %d rows, want all but the unkeyed and unprovable", len(p.Part.Minutes), len(own))
	}
	pub := f.priv.Public().(ed25519.PublicKey)
	c, err := submission.VerifyPart(p.Part, submission.Device{ID: laptop, PublicKey: pub, Records: []submission.EvidenceRecord{f.stored}}, own)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[submission.Result]int{}
	for _, r := range c.Results {
		counts[r.Result]++
		if r.Result == submission.ResultMismatch {
			t.Errorf("mismatch at %s: %+v", r.Row.Minute, r.Server)
		}
	}
	if counts[submission.ResultVerified] != len(own)-2 || counts[submission.ResultUnbacked] != 2 {
		t.Fatalf("results = %v", counts)
	}
}

// The summary says what the part reveals: the rows of each other project
// inside the replay windows (the 08:00 save in lib is outside them), the
// scratch file under no project, the activity samples, and the repo
// folders; and which minutes can't be backed and why.
func TestSummary(t *testing.T) {
	f := newFixture(t)
	s := prepared(t, f).Summary
	if s.Minutes != len(f.own())-2 || s.Credited == 0 || !reflect.DeepEqual(s.Days, []string{"2026-11-20"}) ||
		!reflect.DeepEqual(s.Blocks, []string{"2026-11-20"}) {
		t.Fatalf("summary = %+v", s)
	}
	r := s.Revealed
	if r.Project != (ProjectRows{Project: "app", Heartbeats: 30, Git: 1}) {
		t.Errorf("app rows = %+v", r.Project)
	}
	wantOthers := []ProjectRows{{Project: "", Heartbeats: 1}, {Project: "lib", Heartbeats: 2, AIEvents: 1}}
	if !reflect.DeepEqual(r.Others, wantOthers) {
		t.Errorf("other projects = %+v, want %+v", r.Others, wantOthers)
	}
	if r.Activity != 120 || r.ActiveTime != 0 {
		t.Errorf("activity = %d, active time = %d", r.Activity, r.ActiveTime)
	}
	wantRepos := []evidence.Repo{{Path: "/src/app", Project: "app"}, {Path: "/src/lib", Project: "lib"}}
	if !reflect.DeepEqual(r.Repos, wantRepos) {
		t.Errorf("repos = %+v", r.Repos)
	}
	wantUnbacked := []Count{{What: ReasonNoKey, Minutes: 1}, {What: ReasonUnproven, Minutes: 1}}
	if !reflect.DeepEqual(s.Unbacked, wantUnbacked) {
		t.Errorf("unbacked = %+v", s.Unbacked)
	}
	if !reflect.DeepEqual(s.Others, []Count{{What: "desktop", Minutes: 3}}) {
		t.Errorf("other devices = %+v", s.Others)
	}

	var out bytes.Buffer
	writeSummary(&out, Request{Project: "app", From: "2026-11-20", To: "2026-11-20", Program: "terra"}, s)
	text := out.String()
	for _, want := range []string{
		"Submitting app, 2026-11-20 to 2026-11-20, to program terra.",
		"35 minutes on 1 day",
		"app: 31 rows (30 editor heartbeats, 1 git event)",
		"lib: 3 rows (2 editor heartbeats, 1 AI event)",
		"(no project): 1 row (1 editor heartbeat)",
		"120 activity samples",
		"/src/lib -> lib",
		"1 minute: " + ReasonNoKey,
		"desktop: 3 minutes",
		"Never included: prompt text, code, keystrokes or file contents",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
}

// A minute whose rows carry different keys, or a key whose window doesn't
// hold it, isn't listed: the server would fail the whole part.
func TestPrepareSkipsInconsistentMinutes(t *testing.T) {
	f := newFixture(t)
	other := replay.Key{T: f.key.T, H: f.key.H - 1}
	late := replay.Key{T: at(10, 10, 0), H: f.key.H}
	var split, outside int
	for i, m := range f.plan.Minutes {
		switch {
		case m.DeviceID != laptop || m.ComputedAt == nil || !m.ComputedAt.Equal(f.key.T):
		case m.Minute.Equal(at(10, 20, 0)):
			outside = i
		case m.Minute.Equal(at(10, 5, 0)):
			split = i
		}
	}
	dup := f.plan.Minutes[split]
	dup.Project, dup.ComputedAt, dup.Snapshot = "app", &other.T, &other.H
	f.plan.Minutes = append(f.plan.Minutes, dup)
	f.plan.Minutes[outside].ComputedAt, f.plan.Minutes[outside].Snapshot = &late.T, &late.H

	p := prepared(t, f)
	for _, m := range p.Part.Minutes {
		if m.Minute.Equal(at(10, 5, 0)) || m.Minute.Equal(at(10, 20, 0)) {
			t.Fatalf("listed %s", m.Minute)
		}
	}
	got := map[string]int{}
	for _, c := range p.Summary.Unbacked {
		got[c.What] = c.Minutes
	}
	if got[ReasonKeys] != 1 || got[ReasonBadKey] != 1 {
		t.Fatalf("unbacked = %+v", p.Summary.Unbacked)
	}
}
