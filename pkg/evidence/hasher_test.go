package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// The content hash of the fixture block with fixed salts, and its bucket
// list, are pinned: the agent signs this hash and the server checks proofs
// against it, possibly years later.
func TestContentHashGolden(t *testing.T) {
	blk, list, err := HashBlock(fixtureBlock(), &fixedSalts{})
	if err != nil {
		t.Fatal(err)
	}
	const want = "83511cb6baef42eaece2d915a1adb08676d1656d08875fde10d10a59d1d414be"
	if blk.ContentHash != want {
		t.Errorf("ContentHash = %s, want %s", blk.ContentHash, want)
	}
	from, to := time.Date(2026, 11, 18, 23, 1, 0, 0, time.UTC), time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC)
	if blk.Buckets != 5 || blk.Rows != 12 || !blk.TimeFrom.Equal(from) || !blk.TimeTo.Equal(to) ||
		!maps.Equal(blk.Counts, Counts(fixtureBlock())) {
		t.Errorf("block = %+v (%s..%s)", blk, blk.TimeFrom, blk.TimeTo)
	}
	var buf bytes.Buffer
	for _, b := range list {
		line, _ := json.Marshal(b)
		buf.Write(append(line, '\n'))
	}
	golden(t, "buckets.golden", buf.Bytes())
}

// The hasher agrees with a direct reading of ADR 0019 point 1, written
// independently here: group rows into buckets, hash each, build the RFC
// 6962 tree recursively.
func TestHashMatchesSpec(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	for n := range 60 {
		rows := randomRows(r, n)
		blk, list, err := HashBlock(rows, &fixedSalts{})
		if err != nil {
			t.Fatal(err)
		}
		if got := specRoot(t, rows); got != blk.ContentHash {
			t.Fatalf("%d rows: root %s, spec %s", n, blk.ContentHash, got)
		}
		if root, err := Root(list); err != nil || root.String() != blk.ContentHash || len(list) != blk.Buckets {
			t.Fatalf("Root(list) = %s, %v", root, err)
		}
	}
}

// specRoot computes a content hash from the ADR text with fixed salts.
func specRoot(t *testing.T, rows []Row) string {
	t.Helper()
	buckets := map[uint64][]Row{0: nil}
	for _, r := range rows {
		key := uint64(0)
		if r.Type != TypeRepo {
			key = 1 + uint64(r.Time.Unix()/60)
		}
		buckets[key] = append(buckets[key], r)
	}
	keys := slices.Sorted(maps.Keys(buckets))
	salts := &fixedSalts{}
	var leaves [][]byte
	for _, k := range keys {
		var salt [32]byte
		_, _ = salts.Read(salt[:])
		bs := buckets[k]
		slices.SortFunc(bs, func(a, b Row) int { return int(a.Seq - b.Seq) })
		h := sha256.New()
		h.Write([]byte{2})
		h.Write(salt[:])
		for _, r := range bs {
			line, err := r.Line()
			if err != nil {
				t.Fatal(err)
			}
			h.Write(line)
			h.Write([]byte("\n"))
		}
		leaf := sha256.New()
		leaf.Write([]byte{0})
		_ = binary.Write(leaf, binary.BigEndian, k)
		leaf.Write(h.Sum(nil))
		leaves = append(leaves, leaf.Sum(nil))
	}
	var mth func(d [][]byte) []byte
	mth = func(d [][]byte) []byte {
		if len(d) == 1 {
			return d[0]
		}
		k := 1
		for k*2 < len(d) {
			k *= 2
		}
		h := sha256.New()
		h.Write([]byte{1})
		h.Write(mth(d[:k]))
		h.Write(mth(d[k:]))
		return h.Sum(nil)
	}
	return hex.EncodeToString(mth(leaves))
}

// randomRows returns n rows of random types over a few hours, with unique
// sequence numbers, in random order.
func randomRows(r *rand.Rand, n int) []Row {
	start := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	seqs := r.Perm(n * 2)
	var rows []Row
	for i := range n {
		seq := int64(seqs[i] + 1)
		at := start.Add(time.Duration(r.IntN(4*3600)) * time.Second)
		switch r.IntN(6) {
		case 0:
			rows = append(rows, ActivityRow(seq, wire.ActivitySample{Time: at, IdleSeconds: float64(r.IntN(100)), FocusKind: "editor"}))
		case 1:
			rows = append(rows, HeartbeatRow(seq, wire.Heartbeat{Entity: fmt.Sprintf("/src/f%d.go", r.IntN(5)),
				Time: float64(at.Unix()) + 0.5, Project: "app"}))
		case 2:
			rows = append(rows, AIRow(seq, wire.AIEvent{Time: at, Harness: wire.HarnessCodex, Kind: wire.AIToolStart, SessionID: "s"}))
		case 3:
			rows = append(rows, GitRow(seq, wire.GitEvent{Time: at, Project: "app", Kind: "commit"}))
		case 4:
			rows = append(rows, ActiveTimeRow(seq, ActiveTime{From: at.Add(-time.Minute), To: at, UserSecs: 3}))
		default:
			rows = append(rows, RepoRow(seq, at, Repo{Path: fmt.Sprintf("/src/r%d", r.IntN(3)), Project: "p"}))
		}
	}
	return rows
}

// Hashing a block in chunks of any size, saving and restoring the state
// between them, gives the same block and bucket list as hashing it at once.
func TestHasherChunks(t *testing.T) {
	rows, err := SortForHashing(fixtureBlock())
	if err != nil {
		t.Fatal(err)
	}
	want, wantList, err := HashBlock(rows, &fixedSalts{})
	if err != nil {
		t.Fatal(err)
	}
	for size := 1; size <= len(rows); size++ {
		salts := &fixedSalts{}
		h := NewHasher(salts)
		var list []Bucket
		for i := 0; i < len(rows); i += size {
			closed, err := h.Add(rows[i:min(i+size, len(rows))])
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, closed...)
			saved, err := h.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			h = NewHasher(salts)
			if err := h.UnmarshalBinary(saved); err != nil {
				t.Fatal(err)
			}
		}
		got, rest, err := h.Finish()
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, rest...)
		if fmt.Sprint(got) != fmt.Sprint(want) || !slices.Equal(list, wantList) {
			t.Fatalf("chunks of %d: %+v, want %+v", size, got, want)
		}
	}
}

// A chunk that goes back in (bucket, seq) order is refused whole and
// changes nothing.
func TestHasherRejectsOutOfOrder(t *testing.T) {
	rows, _ := SortForHashing(fixtureBlock())
	want, _, _ := HashBlock(rows, &fixedSalts{})
	h := NewHasher(&fixedSalts{})
	if _, err := h.Add(rows[:5]); err != nil {
		t.Fatal(err)
	}
	for name, chunk := range map[string][]Row{
		"earlier bucket":   {rows[5], rows[0]},
		"same seq":         {rows[4]},
		"lower seq":        {rows[3]},
		"invalid row":      {rows[5], {Seq: 99, Type: "keystroke"}},
		"before 1970":      {rows[5], ActivityRow(98, wire.ActivitySample{Time: time.Unix(-61, 0)})},
		"unencodable time": {rows[5], AIRow(97, wire.AIEvent{Time: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})},
	} {
		if _, err := h.Add(chunk); err == nil {
			t.Fatalf("%s: chunk accepted", name)
		}
	}
	if _, err := h.Add(rows[5:]); err != nil {
		t.Fatal(err)
	}
	got, _, err := h.Finish()
	if err != nil || got.ContentHash != want.ContentHash {
		t.Fatalf("after refused chunks: %s, %v; want %s", got.ContentHash, err, want.ContentHash)
	}
	if _, err := h.Add(nil); !errors.Is(err, ErrHasherDone) {
		t.Fatalf("Add after Finish: %v", err)
	}
	if _, _, err := HashBlock(append(fixtureBlock(), fixtureBlock()[0]), nil); !errors.Is(err, ErrInvalidRow) {
		t.Fatalf("repeated seq: %v", err)
	}
}

// A block always has its repo bucket at index 0, empty when it has no repo
// rows, and no time span when it has nothing else.
func TestRepoBucketAlwaysPresent(t *testing.T) {
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		rows    []Row
		buckets int
		span    bool
	}{
		{"no rows", nil, 1, false},
		{"repo rows only", []Row{RepoRow(1, at, Repo{Path: "/a", Project: "a"})}, 1, false},
		{"no repo rows", []Row{ActivityRow(1, wire.ActivitySample{Time: at}), ActivityRow(2, wire.ActivitySample{Time: at.Add(time.Minute)})}, 3, true},
	}
	for _, tt := range tests {
		blk, list, err := HashBlock(tt.rows, &fixedSalts{})
		if err != nil {
			t.Fatal(err)
		}
		if blk.Buckets != tt.buckets || len(list) != tt.buckets || list[0].Key != RepoKey || (blk.TimeFrom != nil) != tt.span {
			t.Errorf("%s: block %+v, list %+v", tt.name, blk, list)
		}
		if tt.rows == nil {
			want := leafHash(RepoKey, bucketHash(Hash(bytes.Repeat([]byte{1}, 32)), nil))
			if blk.ContentHash != want.String() {
				t.Errorf("empty block hash %s, want %s", blk.ContentHash, want)
			}
		}
	}
}

// Salts come from crypto/rand by default: the same rows hash differently
// every time, so a hidden bucket can't be guessed.
func TestRandomSalts(t *testing.T) {
	a, _, err := HashBlock(fixtureBlock(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := HashBlock(fixtureBlock(), nil)
	if a.ContentHash == b.ContentHash {
		t.Fatal("two blocks with random salts share a content hash")
	}
	if _, _, err := HashBlock(fixtureBlock(), bytes.NewReader(make([]byte, 40))); err == nil {
		t.Fatal("a salt source running dry went unnoticed")
	}
}

func TestFrontierMatchesRoot(t *testing.T) {
	for n := 1; n <= 70; n++ {
		leaves := make([]Hash, n)
		var f frontier
		for i := range leaves {
			leaves[i] = sha256.Sum256([]byte{byte(i)})
			f.add(leaves[i])
		}
		if f.root() != rootOf(leaves) || f.leaves() != uint64(n) || !f.valid() {
			t.Fatalf("n=%d: frontier root differs", n)
		}
	}
}

func TestHasherRejectsBadState(t *testing.T) {
	h := NewHasher(&fixedSalts{})
	if _, err := h.Add(fixtureBlock()[:1]); err != nil {
		t.Fatal(err)
	}
	good, _ := h.MarshalBinary()
	for name, state := range map[string]string{
		"not json":      "{",
		"bad count":     `{"counts":{"keystroke":1}}`,
		"open, no rows": `{"open":{"key":1,"sha256":""},"started":false,"any":false}`,
		"bad tree":      `{"tree":{"nodes":[{"size":3}]}}`,
		"bad sha state": string(bytes.Replace(good, []byte(`"sha256":"`), []byte(`"sha256":"AAAA`), 1)),
	} {
		if err := NewHasher(nil).UnmarshalBinary([]byte(state)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func BenchmarkHashBlockDay(b *testing.B) {
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
		if _, _, err := HashBlock(rows, nil); err != nil {
			b.Fatal(err)
		}
	}
}
