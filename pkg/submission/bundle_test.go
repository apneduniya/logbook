package submission

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// devC is a device with no frozen rows.
const devC = "c0000000-0000-4000-8000-000000000003"

// bundle is the packet fixture as the admin API exports it.
func bundle(t *testing.T) (Bundle, Packet) {
	t.Helper()
	in, f := packetInput(t)
	p, err := BuildPacket(in)
	if err != nil {
		t.Fatal(err)
	}
	var rows, aRows []Minute
	for _, r := range in.Results {
		rows = append(rows, r.Row)
		if r.Row.DeviceID == devA {
			aRows = append(aRows, r.Row)
		}
	}
	desktop := ed25519.NewKeyFromSeed(append(make([]byte, ed25519.SeedSize-1), 2))
	return Bundle{
		Format:     BundleFormat,
		Submission: BundleSubmission{ID: "d0000000-0000-4000-8000-000000000004", Project: "app", From: "2026-11-20", To: "2026-11-20", CreatedAt: at(48, 0)},
		Devices: []BundleDevice{
			{ID: devA, Name: "laptop", PublicKey: f.dev.PublicKey, EvidenceDays: f.dev.Records},
			{ID: devB, Name: "desktop", PublicKey: desktop.Public().(ed25519.PublicKey)},
		},
		Parts:     []Part{f.part(t, partOpts{rows: aRows})},
		Minutes:   rows,
		Capped:    in.Capped,
		CapInputs: in.CapInputs,
	}, p
}

// A bundle survives its JSON round trip and checks to the packet's counts.
func TestCheckBundle(t *testing.T) {
	b, p := bundle(t)
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var back Bundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	rep, err := CheckBundle(back)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Minutes != p.Minutes || rep.CountedSeconds != p.CountedSeconds {
		t.Fatalf("report %+v %d s, packet %+v %d s", rep.Minutes, rep.CountedSeconds, p.Minutes, p.CountedSeconds)
	}
	if len(rep.Results) != len(b.Minutes) || len(rep.Revealed) != 1 {
		t.Fatalf("%d results, %d revealed", len(rep.Results), len(rep.Revealed))
	}
	for _, r := range rep.Results {
		if r.Row.DeviceID == devB && r.Reason != ReasonNoPart {
			t.Fatalf("desktop row %+v", r)
		}
	}
}

func TestCheckBundleRejects(t *testing.T) {
	tests := map[string]struct {
		edit func(b *Bundle)
		want error
	}{
		"another format": {func(b *Bundle) { b.Format = "v0" }, ErrMalformed},
		"a capped minute raised": {func(b *Bundle) {
			b.Capped = slices.Clone(b.Capped)
			b.Capped[0].Seconds++
		}, ErrCapMismatch},
		"the desktop's lib row left out of the cap inputs": {func(b *Bundle) {
			b.CapInputs = slices.DeleteFunc(slices.Clone(b.CapInputs), func(c CapInput) bool { return c.DeviceID == devB && c.Project == "lib" })
		}, ErrCapMismatch},
		"a capped minute with no frozen row": {func(b *Bundle) {
			// The cap inputs agree with it; only the frozen row is missing.
			b.CapInputs = append(slices.Clone(b.CapInputs), CapInput{DeviceID: devA, Minute: at(0, 57), Project: "app"})
			b.Capped = append(slices.Clone(b.Capped), CappedMinute{Minute: at(0, 57)})
		}, ErrCapMismatch},
		"a cap input of the project from a device with no frozen row": {func(b *Bundle) {
			// A third device "credits" 15 s more of the project in a frozen
			// minute; capped agrees with the inputs, but no frozen row
			// backs the extra seconds.
			b.Devices = append(slices.Clone(b.Devices), BundleDevice{ID: devC, Name: "phantom", PublicKey: b.Devices[1].PublicKey})
			b.CapInputs = append(slices.Clone(b.CapInputs), CapInput{DeviceID: devC, Minute: b.Minutes[0].Minute, Project: "app", Credited: 15})
			b.Capped = CapProject(b.CapInputs, "app")
		}, ErrCapMismatch},
		"a cap input at a minute with no frozen row": {func(b *Bundle) {
			b.CapInputs = append(slices.Clone(b.CapInputs), CapInput{DeviceID: devB, Minute: at(0, 57), Project: "lib", Credited: 60})
		}, ErrCapMismatch},
		"another project's negative credit lifting the cap": {func(b *Bundle) {
			i := slices.IndexFunc(b.CapInputs, func(c CapInput) bool { return c.Project == "lib" })
			b.CapInputs = slices.Clone(b.CapInputs)
			b.CapInputs[i].Credited = -60
			b.Capped = CapProject(b.CapInputs, "app")
		}, ErrCapMismatch},
		"a tampered part": {func(b *Bundle) {
			b.Parts = slices.Clone(b.Parts)
			p := &b.Parts[0]
			p.Blocks = slices.Clone(p.Blocks)
			p.Blocks[0].Opened = slices.Clone(p.Blocks[0].Opened)
			o := &p.Blocks[0].Opened[5]
			o.Lines = slices.Clone(o.Lines)
			o.Lines[0] = strings.Replace(o.Lines[0], `"focus_kind":"editor"`, `"focus_kind":"other"`, 1)
		}, ErrMalformed},
		"the laptop's key replaced": {func(b *Bundle) {
			b.Devices = slices.Clone(b.Devices)
			b.Devices[0].PublicKey = b.Devices[1].PublicKey
		}, ErrBadChain},
		"a part of an unknown device": {func(b *Bundle) { b.Devices = b.Devices[:1] }, ErrMalformed},
		"two parts of one device":     {func(b *Bundle) { b.Parts = append(b.Parts, b.Parts[0]) }, ErrMalformed},
		"a device listed twice":       {func(b *Bundle) { b.Devices = append(b.Devices, b.Devices[1]) }, ErrMalformed},
		"a row outside the range": {func(b *Bundle) {
			b.Minutes = slices.Clone(b.Minutes)
			b.Minutes[0].Minute = b.Minutes[0].Minute.Add(-24 * time.Hour)
		}, ErrMalformed},
		"a row of another project": {func(b *Bundle) {
			b.Minutes = slices.Clone(b.Minutes)
			b.Minutes[0].Project = "lib"
		}, ErrMalformed},
	}
	honest, _ := bundle(t)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := honest // each edit clones what it changes
			tt.edit(&b)
			if _, err := CheckBundle(b); !errors.Is(err, tt.want) {
				t.Fatalf("CheckBundle = %v, want %v", err, tt.want)
			}
		})
	}
}

// The upload's JSON is exactly docs/api/submissions.md's: a block is its
// day next to the proof's fields.
func TestUploadJSON(t *testing.T) {
	f := newFixture(t)
	u := Upload{Project: "app", From: "2026-11-20", To: "2026-11-20", Part: f.part(t, partOpts{})}
	if err := u.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Part struct {
			DeviceID string                       `json:"device_id"`
			Blocks   []map[string]json.RawMessage `json:"blocks"`
			Minutes  []map[string]json.RawMessage `json:"minutes"`
		} `json:"part"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]json.RawMessage) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	if got := keys(shape.Part.Blocks[0]); !slices.Equal(got, []string{"buckets", "day", "edges", "hashes", "opened"}) {
		t.Fatalf("block keys = %v", got)
	}
	if got := keys(shape.Part.Minutes[0]); !slices.Equal(got, []string{"computed_at", "minute", "snapshot"}) {
		t.Fatalf("minute keys = %v", got)
	}
	if strings.Contains(string(raw), `"program"`) {
		t.Fatal("an empty program is sent")
	}
	var back Upload
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPart(back.Part, f.dev, f.frozen); err != nil {
		t.Fatalf("the part doesn't verify after a round trip: %v", err)
	}
	for _, bad := range []Upload{
		{Project: "", From: u.From, To: u.To, Part: u.Part},
		{Project: "app", From: "2026-11-21", To: u.To, Part: u.Part},
		{Project: "app", From: u.From, To: u.To, Program: strings.Repeat("p", 65), Part: u.Part},
		{Project: "app", From: u.From, To: u.To},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrMalformed) {
			t.Errorf("Validate(%+v) = %v", bad.Project, err)
		}
	}
}
