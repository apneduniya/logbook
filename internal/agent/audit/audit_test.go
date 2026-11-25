package audit

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	laptop  = "a0000000-0000-4000-8000-000000000001"
	desktop = "b0000000-0000-4000-8000-000000000002"
)

var day = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func at(h, m, s int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

type counter struct{ n int }

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c.n % 251)
		c.n++
	}
	return len(p), nil
}

// goodBundle is a submission of project app on 2026-11-20: the laptop's
// part proves its minutes from one signed block; the desktop has three
// minutes and no part.
func goodBundle(t testing.TB) submission.Bundle {
	t.Helper()
	var rows []evidence.Row
	add := func(row func(seq int64) evidence.Row) { rows = append(rows, row(int64(len(rows)+1))) }
	for _, r := range []evidence.Repo{{Path: "/src/app", Project: "app"}, {Path: "/src/lib", Project: "lib"}} {
		add(func(s int64) evidence.Row { return evidence.RepoRow(s, at(7, 0, 0), r) })
	}
	for ts := at(10, 0, 0); ts.Before(at(10, 20, 0)); ts = ts.Add(15 * time.Second) {
		add(func(s int64) evidence.Row {
			return evidence.ActivityRow(s, wire.ActivitySample{Time: ts, IdleSeconds: 1, FocusApp: "Code", FocusKind: "editor"})
		})
		if ts.Second() == 0 {
			entity := "/src/app/main.go"
			if ts.Minute()%4 == 0 {
				entity = "/src/lib/x.go"
			}
			add(func(s int64) evidence.Row {
				return evidence.HeartbeatRow(s, wire.Heartbeat{Entity: entity, Time: float64(ts.Unix()) + 0.5, IsWrite: true})
			})
		}
	}
	blk, list, err := evidence.HashBlock(rows, &counter{})
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	n := int64(len(rows))
	rec := evidence.Day{DeviceID: laptop, Day: "2026-11-20", ContentHash: blk.ContentHash, RulesVersion: struggle.RulesVersion,
		SeqTo: n, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}
	chain, sig, err := evidence.Sign(priv, rec)
	if err != nil {
		t.Fatal(err)
	}
	key := replay.Key{T: at(11, 0, 0), H: n}
	res, err := replay.Run(rows, key, struggle.RulesVersion, nil)
	if err != nil {
		t.Fatal(err)
	}

	b := submission.Bundle{Format: submission.BundleFormat,
		Submission: submission.BundleSubmission{ID: "c0000000-0000-4000-8000-000000000003", Project: "app",
			From: "2026-11-20", To: "2026-11-20", CreatedAt: day.AddDate(0, 0, 2)},
		Devices: []submission.BundleDevice{
			{ID: laptop, Name: "laptop", PublicKey: priv.Public().(ed25519.PublicKey), EvidenceDays: []submission.EvidenceRecord{{
				Day: rec.Day, ContentHash: rec.ContentHash, ChainHash: chain, Signature: sig, RulesVersion: rec.RulesVersion,
				EventCounts: blk.Counts, SeqTo: n, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}}},
			{ID: desktop, Name: "desktop", PublicKey: ed25519.NewKeyFromSeed(append(make([]byte, 31), 2)).Public().(ed25519.PublicKey)},
		},
	}
	part := submission.Part{DeviceID: laptop}
	appMinutes := map[time.Time]bool{}
	for _, r := range res.Minutes {
		if r.Project == "app" {
			appMinutes[r.Minute] = true
		}
	}
	for _, r := range res.Minutes {
		if r.Project == "" || !appMinutes[r.Minute] {
			continue
		}
		b.CapInputs = append(b.CapInputs, submission.CapInput{DeviceID: laptop, Minute: r.Minute, Project: r.Project, Credited: r.Credited})
		if r.Project != "app" {
			continue
		}
		cuts, flags := slices.Sorted(slices.Values(r.Cuts)), slices.Sorted(slices.Values(r.Flags))
		b.Minutes = append(b.Minutes, submission.Minute{DeviceID: laptop, Minute: r.Minute, Project: "app", Label: string(r.Label),
			Credited: r.Credited, Cuts: append([]string{}, cuts...), Flags: append([]string{}, flags...), RulesVersion: struggle.RulesVersion,
			ComputedAt: &key.T, Snapshot: &key.H})
		part.Minutes = append(part.Minutes, submission.PartMinute{Minute: r.Minute, ComputedAt: key.T, Snapshot: key.H})
	}
	dk := replay.Key{T: at(11, 0, 0), H: 7}
	for _, m := range []int{1, 2, 3} {
		b.Minutes = append(b.Minutes, submission.Minute{DeviceID: desktop, Minute: at(10, m, 0), Project: "app", Label: "you",
			Credited: 60, Cuts: []string{}, Flags: []string{}, RulesVersion: struggle.RulesVersion, ComputedAt: &dk.T, Snapshot: &dk.H})
		b.CapInputs = append(b.CapInputs, submission.CapInput{DeviceID: desktop, Minute: at(10, m, 0), Project: "app", Credited: 60})
	}
	b.Capped = submission.CapProject(b.CapInputs, "app")

	sp := key.Span()
	open, edges := evidence.PlanProof(list, []evidence.MinuteRange{{From: sp.History, To: sp.To}})
	open = append(open, 0)
	edges = slices.DeleteFunc(edges, func(i int) bool { return i == 0 })
	byKey := map[uint64][]evidence.Row{}
	for _, r := range rows {
		k, err := evidence.BucketKey(r)
		if err != nil {
			t.Fatal(err)
		}
		byKey[k] = append(byKey[k], r)
	}
	opened := map[int][]evidence.Row{}
	for _, i := range open {
		opened[i] = byKey[list[i].Key]
	}
	proof, err := evidence.BuildProof(list, opened, edges)
	if err != nil {
		t.Fatal(err)
	}
	part.Blocks = []submission.Block{{Day: rec.Day, Proof: proof}}
	b.Parts = []submission.Part{part}
	return b
}

// clone deep-copies a bundle through JSON, as an auditor would read it.
func clone(t testing.TB, b submission.Bundle) submission.Bundle {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var out submission.Bundle
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGoodBundle(t *testing.T) {
	b := goodBundle(t)
	r := Check(clone(t, b))
	if !r.OK() {
		var out bytes.Buffer
		_ = r.Write(&out)
		t.Fatalf("a good bundle failed:\n%s", out.String())
	}
	laptopRows := len(b.Minutes) - 3
	if r.Minutes.Verified != laptopRows-3 || r.Minutes.Unbacked != 3 || r.Minutes.Mismatch != 0 {
		// The desktop's three minutes are also laptop minutes: a minute
		// counts once, unbacked when any row of it is.
		t.Fatalf("counts = %+v for %d laptop rows", r.Minutes, laptopRows)
	}
	if r.Unbacked[submission.ReasonNoPart] != 3 || r.CountedSeconds == 0 {
		t.Fatalf("unbacked = %v, counted %d", r.Unbacked, r.CountedSeconds)
	}
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"signatures and chain  OK    1 signed record",
		"proofs and replays    OK    1 block, " + strconv.Itoa(laptopRows) + " minutes listed",
		"no part: its 3 frozen rows are unbacked",
		"Cap                     OK",
		"Result                  OK",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// The bundle names its own keys, so the report prints each device's key
// fingerprint and says it wasn't checked; given fingerprints from outside
// the bundle, a key that isn't among them fails, as a bundle re-signed
// with a substituted key would.
func TestPinKeys(t *testing.T) {
	b := goodBundle(t)
	var fps []string
	for _, d := range b.Devices {
		fps = append(fps, evidence.KeyFingerprint(ed25519.PublicKey(d.PublicKey)))
	}
	report := func(r Report) string {
		var out bytes.Buffer
		if err := r.Write(&out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	r := Check(clone(t, b))
	out := report(r)
	if !r.OK() || r.Pinned || !strings.Contains(out, "key                   "+fps[0]+" (not checked") ||
		!strings.Contains(out, "against the keys in the bundle") {
		t.Fatalf("unpinned report:\n%s", out)
	}

	r = Check(clone(t, b))
	r.PinKeys([]string{strings.ToUpper(fps[0]), " " + fps[1] + " "})
	if out := report(r); !r.OK() || !strings.Contains(out, "OK    "+fps[0]) ||
		!strings.Contains(out, "every key, signature") {
		t.Fatalf("pinned report:\n%s", out)
	}

	r = Check(clone(t, b))
	r.PinKeys(fps[:1])
	if out := report(r); r.OK() || !strings.Contains(out, "FAIL  key "+fps[1]+" is none of the fingerprints given") {
		t.Fatalf("report with a key not given:\n%s", out)
	}
}

// Every kind of tampering fails, at the step it belongs to; a frozen row
// the replay disagrees with is reported as a mismatch, not a failure.
func TestTamperedBundles(t *testing.T) {
	tests := map[string]struct {
		edit func(b *submission.Bundle)
		// step is the line that must fail ("" when the bundle still checks).
		step string
	}{
		"flipped signature": {func(b *submission.Bundle) { b.Devices[0].EvidenceDays[0].Signature[0] ^= 1 }, "signatures and chain  FAIL"},
		"another key": {func(b *submission.Bundle) {
			b.Devices[0].PublicKey = b.Devices[1].PublicKey
		}, "signatures and chain  FAIL"},
		"edited line": {func(b *submission.Bundle) {
			o := &b.Parts[0].Blocks[0].Opened[1]
			o.Lines[0] = strings.Replace(o.Lines[0], `"idle_seconds":1`, `"idle_seconds":0`, 1)
		}, "proofs and replays    FAIL"},
		"hidden bucket": {func(b *submission.Bundle) {
			b.Parts[0].Blocks[0].Opened = slices.Delete(b.Parts[0].Blocks[0].Opened, 2, 3)
		}, "proofs and replays    FAIL"},
		"raised cap":        {func(b *submission.Bundle) { b.Capped[0].Seconds++ }, "Cap                     FAIL"},
		"dropped cap input": {func(b *submission.Bundle) { b.CapInputs = b.CapInputs[1:] }, "Cap                     FAIL"},
		"another format":    {func(b *submission.Bundle) { b.Format = "v0" }, "Result                  FAIL"},
		"credit raised everywhere": {func(b *submission.Bundle) {
			i := slices.IndexFunc(b.Minutes, func(m submission.Minute) bool { return m.DeviceID == laptop && m.Label == "you" && m.Credited < 60 })
			if i < 0 {
				i = slices.IndexFunc(b.Minutes, func(m submission.Minute) bool { return m.DeviceID == laptop && m.Credited == 0 })
			}
			m := &b.Minutes[i]
			for j := range b.CapInputs {
				c := &b.CapInputs[j]
				if c.DeviceID == laptop && c.Minute.Equal(m.Minute) && c.Project == "app" {
					c.Credited = m.Credited + 10
				}
			}
			m.Credited += 10
			b.Capped = submission.CapProject(b.CapInputs, "app")
		}, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := clone(t, goodBundle(t))
			tt.edit(&b)
			r := Check(b)
			var out bytes.Buffer
			if err := r.Write(&out); err != nil {
				t.Fatal(err)
			}
			if tt.step == "" {
				if !r.OK() || r.Minutes.Mismatch != 1 || !strings.Contains(out.String(), "mismatch: ") {
					t.Fatalf("want a passing bundle with one mismatch:\n%s", out.String())
				}
				return
			}
			if r.OK() || !strings.Contains(out.String(), tt.step) || !strings.Contains(out.String(), "Result                  FAIL") {
				t.Fatalf("want %q:\n%s", tt.step, out.String())
			}
		})
	}
}

// Read takes the JSON as exported, gzip-compressed or not, whatever the
// file is called.
func TestRead(t *testing.T) {
	b := goodBundle(t)
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"plain": raw, "gzip": gz.Bytes()} {
		got, err := Read(bytes.NewReader(data))
		if err != nil || got.Submission.ID != b.Submission.ID || len(got.Parts) != 1 {
			t.Fatalf("%s: %+v, %v", name, got.Submission, err)
		}
	}
	if _, err := Read(strings.NewReader("{")); err == nil {
		t.Fatal("truncated JSON read")
	}
	if _, err := read(&limitedTest{}, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an endless bundle: %v", err)
	}
}

// limitedTest is an endless JSON string.
type limitedTest struct{ started bool }

func (l *limitedTest) Read(p []byte) (int, error) {
	i := 0
	if !l.started && len(p) > 0 {
		p[0], i, l.started = '"', 1, true
	}
	for ; i < len(p); i++ {
		p[i] = 'a'
	}
	return len(p), nil
}

// logbook verify must check with pkg/ code only (ADR 0020): this package
// imports nothing else of the module.
func TestImportsOnlyPkg(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(path, "github.com/apneduniya/logbook/") && !strings.HasPrefix(path, "github.com/apneduniya/logbook/pkg/") {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
