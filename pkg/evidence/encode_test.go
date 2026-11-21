package evidence

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
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
		RepoRow(2, at(8, 59, 0).In(ist), Repo{Path: "/home/me/app", Project: "app"}),
	}
}

func TestEncodeGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, fixtureBlock()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "block.golden")
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
	got, err := ContentHash(fixtureBlock())
	if err != nil {
		t.Fatal(err)
	}
	const want = "4554779b6cbc919cf92a5c05f3657f017c31d4ccea13cb4ddfadd0f34348a804"
	if got != want {
		t.Fatalf("ContentHash = %s, want %s", got, want)
	}
}

func TestContentHashOrderIndependent(t *testing.T) {
	want, err := ContentHash(fixtureBlock())
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic shuffles for the test
	for i := range 20 {
		rows := fixtureBlock()
		r.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		got, err := ContentHash(rows)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("shuffle %d: hash %s, want %s", i, got, want)
		}
	}
}

func TestContentHashSensitive(t *testing.T) {
	base, _ := ContentHash(fixtureBlock())
	value := func(rows []Row, seq int64, set func(*Row)) {
		for i := range rows {
			if rows[i].Seq == seq {
				set(&rows[i])
			}
		}
	}
	tests := []struct {
		name   string
		mutate func([]Row) []Row
	}{
		{"idle seconds", func(rs []Row) []Row {
			value(rs, 3, func(r *Row) {
				a := r.Value.(wire.ActivitySample)
				a.IdleSeconds = 1.6
				*r = ActivityRow(r.Seq, a)
			})
			return rs
		}},
		{"heartbeat write", func(rs []Row) []Row {
			value(rs, 5, func(r *Row) {
				h := r.Value.(wire.Heartbeat)
				h.IsWrite = false
				*r = HeartbeatRow(r.Seq, h)
			})
			return rs
		}},
		{"active secs", func(rs []Row) []Row {
			value(rs, 7, func(r *Row) {
				a := r.Value.(ActiveTime)
				a.UserSecs = 31
				*r = ActiveTimeRow(r.Seq, a)
			})
			return rs
		}},
		{"repo project", func(rs []Row) []Row {
			value(rs, 2, func(r *Row) { r.Value = Repo{Path: "/home/me/app", Project: "other"} })
			return rs
		}},
		{"sequence number", func(rs []Row) []Row {
			value(rs, 12, func(r *Row) { r.Seq = 13 })
			return rs
		}},
		{"dropped row", func(rs []Row) []Row { return rs[1:] }},
		{"added row", func(rs []Row) []Row {
			return append(rs, GitRow(20, wire.GitEvent{Time: time.Unix(0, 0), Project: "x", Kind: "commit"}))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ContentHash(tt.mutate(fixtureBlock()))
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatal("hash did not change")
			}
		})
	}
}

func TestEncodeRejectsInvalidRows(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]Row) []Row
	}{
		{"zero sequence number", func(rs []Row) []Row { rs[0].Seq = 0; return rs }},
		{"negative sequence number", func(rs []Row) []Row { rs[0].Seq = -3; return rs }},
		{"repeated sequence number", func(rs []Row) []Row { rs[0].Seq = rs[1].Seq; return rs }},
		{"unknown type", func(rs []Row) []Row { rs[0].Type = "keystroke"; return rs }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ContentHash(tt.mutate(fixtureBlock())); !errors.Is(err, ErrInvalidRow) {
				t.Fatalf("ContentHash err = %v, want %v", err, ErrInvalidRow)
			}
		})
	}
}

func TestEmptyBlock(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, nil); err != nil || buf.Len() != 0 {
		t.Fatalf("Encode(empty) = %q, %v", buf.String(), err)
	}
	got, _ := ContentHash(nil)
	if got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty hash = %s, want SHA-256 of nothing", got)
	}
}

func TestCounts(t *testing.T) {
	c := Counts(fixtureBlock())
	want := map[string]int{TypeActivity: 4, TypeActiveTime: 1, TypeAI: 2, TypeGit: 1, TypeHeartbeat: 2, TypeRepo: 1}
	if len(c) != len(want) {
		t.Fatalf("Counts = %v", c)
	}
	for k, v := range want {
		if c[k] != v {
			t.Fatalf("Counts[%s] = %d, want %d", k, c[k], v)
		}
	}
	if empty := Counts(nil); len(empty) != len(want) || empty[TypeRepo] != 0 {
		t.Fatalf("Counts(nil) = %v, want every type at 0", empty)
	}
}

func BenchmarkContentHashDay(b *testing.B) {
	// A busy day: a 5 s activity sample all day, 5000 heartbeats, 2000 AI events.
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	var rows []Row
	seq := int64(0)
	next := func() int64 { seq++; return seq }
	for i := range 17280 {
		rows = append(rows, ActivityRow(next(), wire.ActivitySample{Time: day.Add(time.Duration(i) * 5 * time.Second), FocusKind: "editor", FocusApp: "Code"}))
	}
	for i := range 5000 {
		rows = append(rows, HeartbeatRow(next(), wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(day.Unix() + int64(i*17)), Project: "app"}))
	}
	for i := range 2000 {
		rows = append(rows, AIRow(next(), wire.AIEvent{Time: day.Add(time.Duration(i) * 40 * time.Second), Harness: wire.HarnessCodex, Kind: wire.AIToolStart, SessionID: "s"}))
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ContentHash(rows); err != nil {
			b.Fatal(err)
		}
	}
}

// Hashing a block in chunks, saving and restoring the state between them,
// gives the same content hash and counts as hashing it at once; a chunk
// that goes back in sequence is refused.
func TestHasherChunks(t *testing.T) {
	rows := fixtureBlock()
	want, err := ContentHash(rows)
	if err != nil {
		t.Fatal(err)
	}
	sorted, err := sortedRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHasher()
	for i := 0; i < len(sorted); i += 2 {
		if err := h.Add(sorted[i:min(i+2, len(sorted))]); err != nil {
			t.Fatal(err)
		}
		saved, err := h.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		h = &Hasher{}
		if err := h.UnmarshalBinary(saved); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.Sum(); got != want {
		t.Fatalf("chunked hash = %s, want %s", got, want)
	}
	if got, wantCounts := h.Counts(), Counts(rows); fmt.Sprint(got) != fmt.Sprint(wantCounts) || h.Rows() != len(rows) {
		t.Fatalf("counts = %v (%d rows), want %v", got, h.Rows(), wantCounts)
	}
	if err := h.Add(sorted[:1]); !errors.Is(err, ErrInvalidRow) {
		t.Fatalf("a chunk going back in sequence: %v", err)
	}
	if h.Sum() != want {
		t.Fatal("a refused chunk changed the hash")
	}
}
