package evidence

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func ptr[T any](v T) *T { return &v }

// fixtureBlock has every row type, rows of different days (a block holds
// whatever was logged in its range), the same sample logged twice, a
// non-UTC location and characters encoding/json would HTML-escape by
// default. Rows are listed out of sequence order.
func fixtureBlock() []Row {
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	ist := time.FixedZone("IST", 5*3600+1800)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	return []Row{
		ActivityRow(3, wire.ActivitySample{Time: at(9, 0, 0), IdleSeconds: 1.5, FocusApp: "Code", FocusKind: "editor"}),
		ActivityRow(1, wire.ActivitySample{Time: at(9, 0, 5).In(ist), IdleSeconds: 0, FocusApp: "WindowsTerminal.exe", FocusKind: "terminal"}),
		// The same sample logged twice: only the sequence number tells the
		// two lines apart.
		ActivityRow(4, wire.ActivitySample{Time: at(9, 0, 10), IdleSeconds: 2, FocusApp: "Code", FocusKind: "editor"}),
		ActivityRow(9, wire.ActivitySample{Time: at(9, 0, 10), IdleSeconds: 2, FocusApp: "Code", FocusKind: "editor"}),
		HeartbeatRow(5, wire.Heartbeat{Entity: "/home/me/app/main.go", Type: "file", Category: "coding", Time: float64(at(9, 0, 3).Unix()) + 0.25,
			Project: "app", Language: "Go", Lineno: ptr(12), Cursorpos: ptr(4), IsWrite: true}),
		// From wakatime-cli's offline queue, logged an hour late: it is in
		// the block open when it arrived.
		HeartbeatRow(12, wire.Heartbeat{Entity: "/home/me/app/<b>&.go", Time: float64(at(9, 0, 5).Unix()), Project: "app", AILineChanges: ptr(3)}),
		AIRow(6, wire.AIEvent{Time: at(9, 0, 5), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "s1",
			Project: "app", Cwd: "/home/me/app", PromptChars: 42, PromptHash: "ab12"}),
		// An event of the day before, drained from the hook spool.
		AIRow(10, wire.AIEvent{Time: at(-1, 1, 0).In(ist), Harness: wire.HarnessClaudeCode, Kind: wire.AITurnEnd, SessionID: "s1"}),
		GitRow(11, wire.GitEvent{Time: at(9, 30, 0), Project: "app", Kind: "commit", CommitHash: "0123abcd", FilesChanged: 2, Insertions: 10, Deletions: 1}),
		ActiveTimeRow(7, ActiveTime{From: at(9, 0, 0), To: at(9, 1, 0).In(ist), SessionID: "s1", UserSecs: 30, CLISecs: 12.5,
			Harness: wire.HarnessClaudeCode}),
		// A repo row is in the repo bucket, whatever its time.
		RepoRow(2, at(8, 59, 0).In(ist), Repo{Path: "/home/me/app", Project: "app"}),
		RepoRow(8, at(9, 0, 30), Repo{Path: "/home/me/lib", Project: "lib"}),
	}
}

// fixedSalts is a deterministic salt source for golden tests: its n-th
// salt (from 0) is 32 bytes of n+1.
type fixedSalts struct{ n byte }

func (s *fixedSalts) Read(p []byte) (int, error) {
	for i := range p {
		if i%32 == 0 && i > 0 {
			s.n++
		}
		p[i] = s.n + 1
	}
	if len(p) > 0 {
		s.n++
	}
	return len(p), nil
}

// golden compares got with testdata/name, rewriting it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed (a format change?):\n got: %s\nwant: %s", name, got, want)
	}
}
