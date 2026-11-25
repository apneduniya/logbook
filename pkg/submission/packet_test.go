package submission

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

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
		t.Fatalf("%s differs from the golden file (run go test -update to rewrite it):\n%s", name, got)
	}
}

// packetInput is the fixture as submitted for app on 2026-11-20: device A
// (laptop) backs its minutes, but the server's replay credits 00:15 with
// 60 s where the device said 45 s; device B (desktop) also credited app
// from 00:40 to 00:44 and lib at 00:46, and has added no part.
func packetInput(t *testing.T) (PacketInput, *fixture) {
	t.Helper()
	f := newFixture(t)
	rows := slices.Clone(f.frozen)
	i := slices.IndexFunc(rows, func(r Minute) bool { return r.Minute.Equal(at(0, 15)) })
	if i < 0 || rows[i].Credited != 60 {
		t.Fatalf("the fixture doesn't credit 00:15 with 60 s")
	}
	rows[i].Credited = 45
	c, err := VerifyPart(f.part(t, partOpts{rows: rows}), f.dev, rows)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := rows[0].Key()
	var bRows []Minute
	for m := at(0, 40); m.Before(at(0, 45)); m = m.Add(time.Minute) {
		bRows = append(bRows, Minute{DeviceID: devB, Minute: m, Project: "app", Label: "you", Credited: 60, Cuts: []string{},
			Flags: []string{}, RulesVersion: struggle.RulesVersion, ComputedAt: ptr(k.T.Add(20 * time.Minute)), Snapshot: ptr(int64(900))})
	}
	var inputs []CapInput
	for _, r := range slices.Concat(rows, f.lib, bRows) {
		inputs = append(inputs, CapInput{DeviceID: r.DeviceID, Minute: r.Minute, Project: r.Project, Credited: r.Credited})
	}
	inputs = append(inputs, CapInput{DeviceID: devB, Minute: at(0, 46), Project: "lib", Credited: 60})
	rg, err := ParseRange("2026-11-20", "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	return PacketInput{
		Project:   "app",
		Range:     rg,
		Results:   append(c.Results, Unbacked(bRows, ReasonNoPart)...),
		Capped:    CapProject(inputs, "app"),
		CapInputs: inputs,
		Revealed:  []DeviceRows{{DeviceID: devA, Rows: c.Revealed}},
		Devices: []DeviceInfo{
			{ID: devA, Name: "laptop", OS: "linux", Sealed: []string{"2026-11-19", "2026-11-20"}},
			{ID: devB, Name: "desktop", OS: "windows", Sealed: []string{"2026-11-18"}},
			{ID: "c0000000-0000-4000-8000-000000000003", Name: "unused", OS: "darwin"},
		},
		Raw:     []RawDay{{Day: "2026-11-20", Seconds: 5400, AISeconds: 900}},
		RepoURL: "https://github.com/fiona/app.git",
		Trust:   Trust{AtSubmission: "green", HackatimeLinked: true},
	}, f
}

func TestBuildPacketGolden(t *testing.T) {
	in, _ := packetInput(t)
	p, err := BuildPacket(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "packet.golden", append(b, '\n'))

	// Rebuilding gives the same packet: the re-check job is idempotent.
	again, err := BuildPacket(in)
	if err != nil || !reflect.DeepEqual(again, p) {
		t.Fatalf("a second build differs: %v", err)
	}

	var capped int64
	for _, c := range in.Capped {
		capped += c.Seconds
	}
	if p.CountedSeconds != capped || p.MadeOf.Editing+p.MadeOf.WithAI+p.MadeOf.Other != capped {
		t.Fatalf("counted %d, made of %+v, capped %d", p.CountedSeconds, p.MadeOf, capped)
	}
	if p.MadeOf.Editing == 0 || p.MadeOf.WithAI == 0 {
		t.Fatalf("made of = %+v: the fixture has editing and AI minutes", p.MadeOf)
	}
	if p.Minutes.Mismatch != 1 || p.Minutes.Unbacked != 5 || p.Minutes.Verified == 0 {
		t.Fatalf("minutes = %+v", p.Minutes)
	}
	if p.OneThirdRuleSeconds != 5400-900+300 {
		t.Fatalf("one third rule = %d", p.OneThirdRuleSeconds)
	}
	day := p.Days[0]
	if len(day.Strip) != 24*60 || day.Strip[15] != StripMismatch || day.Strip[40] != StripUnbacked ||
		day.Strip[10] != StripCounted || day.Strip[30] != StripAgentAlone || day.Strip[600] != StripIdle {
		t.Fatalf("strip = %s", day.Strip[:70])
	}
	if len(day.Commits) != 1 || day.Commits[0].URL !=
		"https://github.com/fiona/app/commit/0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("commits = %+v", day.Commits)
	}
	// 00:40 to 00:44 (app on both) and 00:46 (lib on the desktop).
	if !slices.ContainsFunc(p.Flags, func(f FlagSummary) bool { return f.Code == struggle.FlagMultiDevice && f.Minutes == 6 }) {
		t.Fatalf("flags = %+v: six minutes were credited on two devices", p.Flags)
	}
	if len(p.Devices) != 2 || !slices.Equal(p.Devices[0].Gaps, []string{}) || !slices.Equal(p.Devices[1].Gaps, []string{"2026-11-20"}) {
		t.Fatalf("devices = %+v", p.Devices)
	}
}

// The packet never holds file paths, other projects, prompt hashes or
// session ids, though the revealed rows have all four.
func TestPacketKeepsPrivateFieldsOut(t *testing.T) {
	in, _ := packetInput(t)
	p, err := BuildPacket(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"/src", "main.go", `"lib"`, "deadbeef", `"s1"`, "session_id", "Code", devA, devB} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the packet contains %q", secret)
		}
	}
	revealed := false
	for _, r := range in.Revealed[0].Rows {
		l, _ := r.Line()
		revealed = revealed || strings.Contains(string(l), "/src/lib")
	}
	if !revealed {
		t.Fatal("the revealed rows hold no other project: the test checks nothing")
	}
}

func TestPacketListsAtMost200MismatchesNewestFirst(t *testing.T) {
	var res []RowResult
	for i := range 250 {
		res = append(res, RowResult{Row: Minute{DeviceID: devA, Minute: at(1, i), Project: "app", Label: "you", Credited: 60,
			RulesVersion: "v1"}, Result: ResultMismatch, Server: &Credit{Label: "agent", Cuts: []string{"agent_alone"}}})
	}
	rg, _ := ParseRange("2026-11-20", "2026-11-20")
	p, err := BuildPacket(PacketInput{Project: "app", Range: rg, Results: res})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Mismatches) != MaxMismatches || !p.Mismatches[0].Minute.Equal(at(1, 249)) || !p.Mismatches[199].Minute.Equal(at(1, 50)) {
		t.Fatalf("%d mismatches, first %v, last %v", len(p.Mismatches), p.Mismatches[0].Minute, p.Mismatches[len(p.Mismatches)-1].Minute)
	}
	if p.Minutes.Mismatch != 250 || p.Mismatches[0].Server.Label != "agent" || p.Mismatches[0].Device.Credited != 60 {
		t.Fatalf("minutes %+v, first %+v", p.Minutes, p.Mismatches[0])
	}
}

func TestBuildPacketRejectsRowsOutsideTheRange(t *testing.T) {
	rg, _ := ParseRange("2026-11-21", "2026-11-21")
	if _, err := BuildPacket(PacketInput{Project: "app", Range: rg, Results: []RowResult{{Row: Minute{Minute: at(1, 0)}}}}); err == nil {
		t.Fatal("a row outside the range was accepted")
	}
	if _, err := BuildPacket(PacketInput{Project: "app", Range: rg, Capped: []CappedMinute{{Minute: at(1, 0)}}}); err == nil {
		t.Fatal("a capped minute outside the range was accepted")
	}
}

func TestCommitURL(t *testing.T) {
	const h = "0123abcd"
	tests := map[string]string{
		"https://github.com/fiona/app":         "https://github.com/fiona/app/commit/" + h,
		"https://github.com/fiona/app.git":     "https://github.com/fiona/app/commit/" + h,
		"https://www.github.com/fiona/app/":    "https://github.com/fiona/app/commit/" + h,
		"http://github.com/fiona/app":          "",
		"https://gitlab.com/fiona/app":         "",
		"https://github.com/fiona":             "",
		"https://github.com/fiona/app/tree/x":  "",
		"https://github.com/fi%2Fona/app":      "",
		"https://github.com/fiona/app?x=1":     "",
		"https://user@github.com/fiona/app":    "",
		"javascript:alert(1)//github.com/a/b":  "",
		"":                                     "",
		"https://github.com/fiona/..":          "",
		"https://github.com.evil.com/fiona/ap": "",
	}
	for repo, want := range tests {
		if got := commitURL(repo, h); got != want {
			t.Errorf("commitURL(%q) = %q, want %q", repo, got, want)
		}
	}
	if got := commitURL("https://github.com/fiona/app", "abc/../x"); got != "" {
		t.Errorf("a hash with a slash gave %q", got)
	}
}

func TestCommitHash(t *testing.T) {
	for h, want := range map[string]bool{
		"0123abc": true, strings.Repeat("a", 40): true, strings.Repeat("f", 64): true,
		"0123ab": false, strings.Repeat("a", 65): false, "0123ABC": false, "0123abg": false, "<script>": false, "": false,
	} {
		if got := commitHash(h); got != want {
			t.Errorf("commitHash(%q) = %v", h, got)
		}
	}
}

func TestJustification(t *testing.T) {
	p := Packet{RulesVersion: "v1", CountedSeconds: 44640, RawSeconds: 61200, OneThirdRuleSeconds: 55200,
		Minutes: Counts{Verified: 2431}}
	want := "Tracked with Logbook (rules v1). 12.4 h counted of 17.0 h at the keyboard; the one-third AI rule would give 15.3 h. " +
		"All 2,431 minutes re-computed on our server from signed device logs. Review: https://logbook.boogles.dev/r/abc"
	if got := Justification(p, "https://logbook.boogles.dev/r/abc"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	p.Minutes = Counts{Verified: 1000, Mismatch: 2, Unbacked: 1234}
	want = "Tracked with Logbook (rules v1). 12.4 h counted of 17.0 h at the keyboard; the one-third AI rule would give 15.3 h. " +
		"1,000 of 2,236 minutes re-computed on our server from signed device logs; 2 differ from the device's result and 1,234 are not yet backed by evidence. Review: L"
	if got := Justification(p, "L"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if p.Justification != "" {
		t.Fatal("Justification changed the packet")
	}
}
