package replay

import (
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// t0 is a run's T in the fixture: 12:00 UTC.
var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func at(hhmm string) time.Time {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		panic(err)
	}
	return t0.Add(time.Duration(t.Hour()-12)*time.Hour + time.Duration(t.Minute())*time.Minute)
}

// session is a person working in app 10:30..11:50, away 11:25..11:36
// while an AI turn runs, with a repo mapping that changes and rows the key
// must not see.
func session() []evidence.Row {
	var rows []evidence.Row
	seq := int64(0)
	next := func() int64 { seq++; return seq }
	rows = append(rows, evidence.RepoRow(next(), at("09:00"), evidence.Repo{Path: "/src/app", Project: "old"}))
	rows = append(rows, evidence.RepoRow(next(), at("09:00"), evidence.Repo{Path: "/src/app", Project: "app"}))
	for s := at("10:30"); s.Before(at("11:50")); s = s.Add(5 * time.Second) {
		if !s.Before(at("11:25")) && s.Before(at("11:36")) {
			continue // away from the keyboard
		}
		rows = append(rows, evidence.ActivityRow(next(), wire.ActivitySample{Time: s, IdleSeconds: 1, FocusApp: "Code", FocusKind: "editor"}))
		if s.Second() == 0 {
			rows = append(rows, evidence.HeartbeatRow(next(), wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(s.Unix()) + 0.5,
				Project: "app", IsWrite: s.Minute()%3 == 0}))
		}
	}
	rows = append(rows,
		evidence.AIRow(next(), wire.AIEvent{Time: at("11:24"), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit,
			SessionID: "s", Cwd: "/src/app", PromptChars: 60, PromptHash: "aa"}),
		evidence.AIRow(next(), wire.AIEvent{Time: at("11:27"), Harness: wire.HarnessClaudeCode, Kind: wire.AIToolStart, SessionID: "s", Cwd: "/src/app"}),
		evidence.AIRow(next(), wire.AIEvent{Time: at("11:33"), Harness: wire.HarnessClaudeCode, Kind: wire.AIToolEnd, SessionID: "s", Cwd: "/src/app"}),
		evidence.AIRow(next(), wire.AIEvent{Time: at("11:36"), Harness: wire.HarnessClaudeCode, Kind: wire.AITurnEnd, SessionID: "s"}),
		evidence.GitRow(next(), wire.GitEvent{Time: at("11:30"), Project: "app", Kind: "commit"}),
		evidence.ActiveTimeRow(next(), evidence.ActiveTime{From: at("11:00"), To: at("11:01"), SessionID: "s", UserSecs: 40}),
	)
	return rows
}

func TestSpanAndKey(t *testing.T) {
	s := SpanAt(t0.Add(30 * time.Second))
	if !s.To.Equal(t0) || !s.From.Equal(at("11:00")) || !s.History.Equal(at("10:00")) {
		t.Fatalf("SpanAt = %+v", s)
	}
	for _, k := range []Key{{}, {T: t0.Add(time.Second), H: 1}, {T: t0, H: -1}} {
		if err := k.Validate(); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Validate(%+v) = %v", k, err)
		}
	}
	if err := (Key{T: t0, H: 0}).Validate(); err != nil {
		t.Fatalf("an empty log's key: %v", err)
	}
}

// Input reads exactly the rows of the key: seq <= H, time in [T - 120 min,
// T), engine types only, and the repo mapping reduced at H.
func TestInputSelectsTheKeysRows(t *testing.T) {
	a := func(seq int64, t time.Time) evidence.Row {
		return evidence.ActivityRow(seq, wire.ActivitySample{Time: t, FocusKind: "editor"})
	}
	rows := []evidence.Row{
		a(1, at("10:00").Add(-time.Millisecond)), // before the history
		a(2, at("10:00")),                        // the first instant read
		a(3, t0.Add(-time.Millisecond)),          // the last
		a(4, t0),                                 // T itself: not read
		a(20, at("11:30")),                       // after H
		evidence.HeartbeatRow(5, wire.Heartbeat{Entity: "/src/app/x.go", Time: float64(at("11:00").Unix()), Project: "app", AIModel: "m"}),
		evidence.AIRow(6, wire.AIEvent{Time: at("11:00"), Kind: wire.AIPromptSubmit, SessionID: "s"}),
		evidence.GitRow(7, wire.GitEvent{Time: at("11:00"), Project: "app", Kind: "commit"}),
		evidence.ActiveTimeRow(8, evidence.ActiveTime{From: at("10:59"), To: at("11:00"), UserSecs: 1}),
		evidence.RepoRow(9, at("08:00"), evidence.Repo{Path: "/src/b", Project: "b"}),
		evidence.RepoRow(10, at("08:00"), evidence.Repo{Path: "/src/a", Project: "z"}),
		evidence.RepoRow(11, at("08:00"), evidence.Repo{Path: "/src/a", Project: "a"}),
		evidence.RepoRow(21, at("08:00"), evidence.Repo{Path: "/src/a", Project: "later"}),
	}
	in, err := Input(rows, Key{T: t0, H: 12}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Window.From.Equal(at("11:00")) || !in.Window.To.Equal(t0) || !in.DataEnd.Equal(t0) || in.TZ != time.UTC {
		t.Fatalf("window %+v, data end %v, tz %v", in.Window, in.DataEnd, in.TZ)
	}
	if len(in.Activity) != 2 || !in.Activity[0].Time.Equal(at("10:00")) || len(in.Heartbeats) != 1 || !in.Heartbeats[0].AI ||
		len(in.AI) != 1 || in.ActiveTime != nil {
		t.Fatalf("input = %+v", in)
	}
	if want := []struggle.Repo{{Path: "/src/a", Project: "a"}, {Path: "/src/b", Project: "b"}}; !reflect.DeepEqual(in.Repos, want) {
		t.Fatalf("repos = %+v, want %+v", in.Repos, want)
	}
	if _, err := Input(rows, Key{T: t0.Add(time.Second), H: 12}, nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("key off a minute: %v", err)
	}
	bad := []evidence.Row{{Seq: 1, Type: evidence.TypeAI, Time: at("11:00"), Value: wire.GitEvent{}}}
	if _, err := Input(bad, Key{T: t0, H: 1}, nil); err == nil {
		t.Fatal("a row holding the wrong value was accepted")
	}
}

// The order rows come in doesn't change the input, and heartbeats in one
// millisecond come in sequence order, as the agent's log reads them.
func TestInputOrder(t *testing.T) {
	rows := session()
	ms := float64(at("11:40").Unix())
	rows = append(rows,
		evidence.HeartbeatRow(1000, wire.Heartbeat{Entity: "/src/app/b.go", Time: ms + 0.0009, Project: "app"}),
		evidence.HeartbeatRow(1001, wire.Heartbeat{Entity: "/src/app/a.go", Time: ms + 0.0001, Project: "app"}),
	)
	key := Key{T: t0, H: 2000}
	want, err := Input(rows, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 10 {
		r.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		got, err := Input(rows, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("input depends on row order")
		}
	}
	var entities []string
	for _, h := range want.Heartbeats {
		if h.Time.Unix() == at("11:40").Unix() && strings.HasSuffix(h.Entity, ".go") && h.Entity != "/src/app/main.go" {
			entities = append(entities, h.Entity)
		}
	}
	if !reflect.DeepEqual(entities, []string{"/src/app/b.go", "/src/app/a.go"}) {
		t.Fatalf("same-millisecond heartbeats in order %v, want sequence order", entities)
	}
}

// Repos takes the latest row per exact path at H, whatever the project
// names; the engine would pick by name.
func TestRepos(t *testing.T) {
	rows := []evidence.Row{
		evidence.RepoRow(3, t0, evidence.Repo{Path: "/src/app", Project: "aaa"}),
		evidence.RepoRow(1, t0, evidence.Repo{Path: "/src/app", Project: "zzz"}),
		evidence.RepoRow(2, t0, evidence.Repo{Path: "/src/app/", Project: "slash"}),
		evidence.RepoRow(4, t0, evidence.Repo{Path: "/src/app", Project: "after"}),
	}
	want := []struggle.Repo{{Path: "/src/app", Project: "aaa"}, {Path: "/src/app/", Project: "slash"}}
	if got := Repos(rows, 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("Repos = %+v, want %+v", got, want)
	}
	if got := Repos(rows, 0); len(got) != 0 {
		t.Fatalf("Repos at 0 = %+v", got)
	}
}

func TestIsAIHeartbeat(t *testing.T) {
	n := 3
	var tokens int64 = 5
	tests := []struct {
		h    wire.Heartbeat
		want bool
	}{
		{wire.Heartbeat{Category: "coding"}, false},
		{wire.Heartbeat{Category: "AI Coding"}, true},
		{wire.Heartbeat{AIModel: "m"}, true},
		{wire.Heartbeat{AISession: "s"}, true},
		{wire.Heartbeat{AIInputTokens: &tokens}, true},
		{wire.Heartbeat{AILineChanges: &n}, true},
		{wire.Heartbeat{HumanLineChanges: &n}, false},
	}
	for i, tt := range tests {
		if got := AIHeartbeat(tt.h); got != tt.want {
			t.Errorf("case %d: AIHeartbeat = %v", i, got)
		}
	}
}

// The replay of the fixture session at its key, pinned: what the server's
// re-check computes from these rows.
func TestRunGolden(t *testing.T) {
	res, err := Run(session(), Key{T: t0, H: 1000}, struggle.RulesVersion, nil)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for m := at("11:00"); m.Before(t0); m = m.Add(time.Minute) {
		for _, c := range Credits(res, m) {
			fmt.Fprintf(&b, "%s %s %s %d %v\n", m.Format("15:04"), c.Project, c.Label, c.Credited, c.Cuts)
		}
	}
	path := filepath.Join("testdata", "run.golden")
	if *update {
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("replay changed:\n got:\n%s\nwant:\n%s", b.String(), want)
	}
	if _, err := Run(session(), Key{T: t0, H: 1000}, "v0", nil); !errors.Is(err, ErrUnknownRules) {
		t.Fatalf("unknown rules: %v", err)
	}
}

// What the server will do (ADR 0020): seal the rows, open the window of a
// key and the repo bucket with a proof, verify it, and build the input
// from the revealed rows alone. It equals the input from the whole log.
func TestInputFromAProof(t *testing.T) {
	rows := session()
	key := Key{T: t0, H: int64(len(rows))}
	salts := rand.NewChaCha8([32]byte{1})
	blk, list, err := evidence.HashBlock(rows, salts)
	if err != nil {
		t.Fatal(err)
	}
	d := evidence.Day{ContentHash: blk.ContentHash, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo,
		SeqFrom: 0, SeqTo: int64(len(rows))}
	s := key.Span()
	window := evidence.MinuteRange{From: s.History, To: s.To}
	open, edges := evidence.PlanProof(list, []evidence.MinuteRange{window})
	open = append([]int{0}, open...)
	byIndex := map[int][]evidence.Row{}
	for _, r := range rows {
		k, err := evidence.BucketKey(r)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range list {
			if b.Key == k {
				byIndex[i] = append(byIndex[i], r)
			}
		}
	}
	opened := map[int][]evidence.Row{}
	for _, i := range open {
		opened[i] = byIndex[i]
	}
	p, err := evidence.BuildProof(list, opened, edges)
	if err != nil {
		t.Fatal(err)
	}
	v, err := evidence.VerifyProof(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.NeedsCompleteness(d, window, key.H) || v.CheckComplete(window) != nil {
		t.Fatal("the window isn't proven complete")
	}
	got, err := Input(v.Rows(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Input(rows, key, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("input from the proof differs from the input from the log")
	}
}

func TestSame(t *testing.T) {
	you := Credit{Project: "app", Label: "you", Credited: 60}
	cut := Credit{Project: "lib", Label: "you", Credited: 0, Cuts: []string{"rubber_stamp", "loop"}}
	nothing := Credit{Label: "idle"}
	tests := []struct {
		name string
		a, b []Credit
		want bool
	}{
		{"equal", []Credit{you, cut}, []Credit{cut, you}, true},
		{"cuts in another order", []Credit{cut}, []Credit{{Project: "lib", Label: "you", Cuts: []string{"loop", "rubber_stamp"}}}, true},
		{"a row that says nothing", []Credit{you, nothing}, []Credit{you}, true},
		{"none", nil, []Credit{}, true},
		{"credit", []Credit{you}, []Credit{{Project: "app", Label: "you", Credited: 59}}, false},
		{"label", []Credit{you}, []Credit{{Project: "app", Label: "agent", Credited: 60}}, false},
		{"project", []Credit{you}, []Credit{{Project: "other", Label: "you", Credited: 60}}, false},
		{"cuts", []Credit{cut}, []Credit{{Project: "lib", Label: "you", Cuts: []string{"loop"}}}, false},
		{"missing row", []Credit{you, cut}, []Credit{you}, false},
	}
	for _, tt := range tests {
		if got := Same(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: Same = %v", tt.name, got)
		}
	}
	if cut.Cuts[0] != "rubber_stamp" {
		t.Fatal("Same sorted the caller's cuts")
	}
}
