package evidence

import (
	"bytes"
	"errors"
	"flag"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func ptr[T any](v T) *T { return &v }

// fixtureDay has every event type, ties on time, a non-UTC location and
// characters encoding/json would HTML-escape by default.
func fixtureDay() DayEvents {
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	ist := time.FixedZone("IST", 5*3600+1800)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	return DayEvents{
		Day: "2026-11-19",
		Activity: []wire.ActivitySample{
			{Time: at(9, 0, 0), IdleSeconds: 1.5, FocusApp: "Code", FocusKind: "editor"},
			{Time: at(9, 0, 5).In(ist), IdleSeconds: 0, FocusApp: "WindowsTerminal.exe", FocusKind: "terminal"},
			{Time: at(9, 0, 5), IdleSeconds: 0, FocusApp: "Code", FocusKind: "editor"},
		},
		Heartbeats: []wire.Heartbeat{
			{Entity: "/home/me/app/main.go", Type: "file", Category: "coding", Time: float64(at(9, 0, 3).Unix()) + 0.25,
				Project: "app", Language: "Go", Lineno: ptr(12), Cursorpos: ptr(4), IsWrite: true},
			{Entity: "/home/me/app/<b>&.go", Time: float64(at(9, 0, 5).Unix()), Project: "app", AILineChanges: ptr(3)},
		},
		AI: []wire.AIEvent{
			{Time: at(9, 0, 5), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "s1",
				Project: "app", Cwd: "/home/me/app", PromptChars: 42, PromptHash: "ab12"},
			{Time: at(9, 1, 0).In(ist), Harness: wire.HarnessClaudeCode, Kind: wire.AITurnEnd, SessionID: "s1"},
		},
		Git: []wire.GitEvent{
			{Time: at(9, 30, 0), Project: "app", Kind: "commit", CommitHash: "0123abcd", FilesChanged: 2, Insertions: 10, Deletions: 1},
		},
		ActiveTime: []ActiveTime{
			{From: at(9, 0, 0), To: at(9, 1, 0), SessionID: "s1", UserSecs: 30, CLISecs: 12.5,
				Harness: wire.HarnessClaudeCode, ProjectCwd: "/home/me/app"},
		},
	}
}

func TestEncodeGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, fixtureDay()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "day.golden")
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Encode changed (format change? see the Encode doc):\n got: %s\nwant: %s", buf.Bytes(), want)
	}
}

func TestContentHashGolden(t *testing.T) {
	// Pinned: the agent signs this hash and the server re-derives it from
	// submitted bundles, possibly years later. If this fails, the canonical
	// encoding or a wire struct's field order changed.
	got, err := ContentHash(fixtureDay())
	if err != nil {
		t.Fatal(err)
	}
	const want = "c627b5619c17ff1164bdb6bfcae1f6a51f9286b6eb8933615d0066bc19b5e648"
	if got != want {
		t.Fatalf("ContentHash = %s, want %s", got, want)
	}
}

func TestContentHashOrderIndependent(t *testing.T) {
	want, err := ContentHash(fixtureDay())
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic shuffles for the test
	for i := range 20 {
		d := fixtureDay()
		r.Shuffle(len(d.Activity), func(i, j int) { d.Activity[i], d.Activity[j] = d.Activity[j], d.Activity[i] })
		r.Shuffle(len(d.Heartbeats), func(i, j int) { d.Heartbeats[i], d.Heartbeats[j] = d.Heartbeats[j], d.Heartbeats[i] })
		r.Shuffle(len(d.AI), func(i, j int) { d.AI[i], d.AI[j] = d.AI[j], d.AI[i] })
		got, err := ContentHash(d)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("shuffle %d: hash %s, want %s", i, got, want)
		}
	}
}

func TestContentHashSensitive(t *testing.T) {
	base, _ := ContentHash(fixtureDay())
	tests := []struct {
		name   string
		mutate func(*DayEvents)
	}{
		{"idle seconds", func(d *DayEvents) { d.Activity[0].IdleSeconds = 1.6 }},
		{"heartbeat write", func(d *DayEvents) { d.Heartbeats[0].IsWrite = false }},
		{"prompt hash", func(d *DayEvents) { d.AI[0].PromptHash = "ab13" }},
		{"git deletions", func(d *DayEvents) { d.Git[0].Deletions = 2 }},
		{"active secs", func(d *DayEvents) { d.ActiveTime[0].UserSecs = 31 }},
		{"dropped event", func(d *DayEvents) { d.Activity = d.Activity[1:] }},
		{"duplicated event", func(d *DayEvents) { d.Git = append(d.Git, d.Git[0]) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fixtureDay()
			tt.mutate(&d)
			got, err := ContentHash(d)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatal("hash did not change")
			}
		})
	}
}

func TestEncodeRejectsEventsOutsideDay(t *testing.T) {
	end := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*DayEvents)
		want   error
	}{
		{"activity at next midnight", func(d *DayEvents) { d.Activity[0].Time = end }, ErrOutsideDay},
		{"ai before day", func(d *DayEvents) { d.AI[0].Time = end.AddDate(0, 0, -1).Add(-time.Nanosecond) }, ErrOutsideDay},
		{"heartbeat next day", func(d *DayEvents) { d.Heartbeats[0].Time = float64(end.Unix()) + 1 }, ErrOutsideDay},
		{"active time ending next day", func(d *DayEvents) { d.ActiveTime[0].To = end }, ErrOutsideDay},
		{"local time on the next UTC day", func(d *DayEvents) {
			d.Git[0].Time = time.Date(2026, 11, 20, 1, 0, 0, 0, time.FixedZone("CET", 3600)) // 00:00 UTC
		}, ErrOutsideDay},
		{"bad day", func(d *DayEvents) { d.Day = "19/11/2026" }, ErrInvalidDay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fixtureDay()
			tt.mutate(&d)
			if _, err := ContentHash(d); !errors.Is(err, tt.want) {
				t.Fatalf("ContentHash err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEmptyDay(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, DayEvents{Day: "2026-11-19"}); err != nil || buf.Len() != 0 {
		t.Fatalf("Encode(empty) = %q, %v", buf.String(), err)
	}
	got, _ := ContentHash(DayEvents{Day: "2026-11-19"})
	if got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty hash = %s, want SHA-256 of nothing", got)
	}
}

func TestCounts(t *testing.T) {
	c := Counts(fixtureDay())
	want := map[string]int{TypeActivity: 3, TypeActiveTime: 1, TypeAI: 2, TypeGit: 1, TypeHeartbeat: 2}
	if len(c) != len(want) {
		t.Fatalf("Counts = %v", c)
	}
	for k, v := range want {
		if c[k] != v {
			t.Fatalf("Counts[%s] = %d, want %d", k, c[k], v)
		}
	}
}

func BenchmarkContentHashDay(b *testing.B) {
	// A busy day: a 5 s activity sample all day, 5000 heartbeats, 2000 AI events.
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	d := DayEvents{Day: "2026-11-19"}
	for i := range 17280 {
		d.Activity = append(d.Activity, wire.ActivitySample{Time: day.Add(time.Duration(i) * 5 * time.Second), FocusKind: "editor", FocusApp: "Code"})
	}
	for i := range 5000 {
		d.Heartbeats = append(d.Heartbeats, wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(day.Unix() + int64(i*17)), Project: "app"})
	}
	for i := range 2000 {
		d.AI = append(d.AI, wire.AIEvent{Time: day.Add(time.Duration(i) * 40 * time.Second), Harness: wire.HarnessCodex, Kind: wire.AIToolStart, SessionID: "s"})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ContentHash(d); err != nil {
			b.Fatal(err)
		}
	}
}
