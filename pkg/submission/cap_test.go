package submission

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

var capMinute = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func capAt(i int) time.Time { return capMinute.Add(time.Duration(i) * time.Minute) }

// capCase is one case of testdata/cap_cases.json, which the test of the
// credited_project_minutes view (internal/server/credited) runs too, so
// Cap and the view are checked on the same cases.
type capCase struct {
	Name   string          `json:"name"`
	Inputs []CapInput      `json:"inputs"`
	Want   []CappedProject `json:"want"`
}

func TestCap(t *testing.T) {
	b, err := os.ReadFile("testdata/cap_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []capCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			if got := Cap(tt.Inputs); !reflect.DeepEqual(got, tt.Want) {
				t.Fatalf("Cap = %+v\nwant %+v", got, tt.Want)
			}
		})
	}
}

func TestCapProject(t *testing.T) {
	in := []CapInput{
		{DeviceID: "a", Minute: capAt(0), Project: "app", Credited: 60},
		{DeviceID: "b", Minute: capAt(0), Project: "lib", Credited: 30},
		{DeviceID: "a", Minute: capAt(1), Project: "lib", Credited: 60},
		{DeviceID: "a", Minute: capAt(2), Project: "app", Credited: 0},
	}
	want := []CappedMinute{{Minute: capAt(0), Seconds: 40}, {Minute: capAt(2), Seconds: 0}}
	if got := CapProject(in, "app"); !reflect.DeepEqual(got, want) {
		t.Fatalf("CapProject = %+v, want %+v", got, want)
	}
}

func TestCheckCap(t *testing.T) {
	ck, sn := capAt(30), int64(7)
	row := func(dev string, i, credited int) Minute {
		return Minute{DeviceID: dev, Minute: capAt(i), Project: "app", Label: "you", Credited: credited,
			RulesVersion: "v1", ComputedAt: &ck, Snapshot: &sn}
	}
	inputs := []CapInput{
		{DeviceID: "a", Minute: capAt(0), Project: "app", Credited: 60},
		{DeviceID: "b", Minute: capAt(0), Project: "app", Credited: 45},
		{DeviceID: "b", Minute: capAt(0), Project: "lib", Credited: 15},
		{DeviceID: "a", Minute: capAt(1), Project: "app", Credited: 30},
	}
	rows := []Minute{row("a", 0, 60), row("b", 0, 45), row("a", 1, 30)}
	capped := []CappedMinute{{Minute: capAt(1), Seconds: 30}, {Minute: capAt(0), Seconds: 52}}
	if err := CheckCap("app", rows, capped, inputs); err != nil {
		t.Fatalf("honest cap: %v", err)
	}
	tests := map[string]func() ([]Minute, []CappedMinute, []CapInput){
		"capped seconds raised": func() ([]Minute, []CappedMinute, []CapInput) {
			return rows, []CappedMinute{{Minute: capAt(1), Seconds: 30}, {Minute: capAt(0), Seconds: 60}}, inputs
		},
		"other device's row left out of the inputs": func() ([]Minute, []CappedMinute, []CapInput) {
			// Without lib's 15 s the minute caps app at 60 * 105 / 105.
			return rows, capped, inputs[:2:2]
		},
		"a capped minute missing": func() ([]Minute, []CappedMinute, []CapInput) {
			return rows, capped[:1], inputs
		},
		"a capped minute with no inputs": func() ([]Minute, []CappedMinute, []CapInput) {
			return rows, append(capped, CappedMinute{Minute: capAt(5), Seconds: 0}), inputs
		},
		"a frozen row not in the inputs": func() ([]Minute, []CappedMinute, []CapInput) {
			return append(rows, row("c", 1, 0)), capped, inputs
		},
		"a frozen row's credit differs": func() ([]Minute, []CappedMinute, []CapInput) {
			return []Minute{row("a", 0, 59), row("b", 0, 45), row("a", 1, 30)}, capped, inputs
		},
		"an input of the project with no frozen row": func() ([]Minute, []CappedMinute, []CapInput) {
			// One verified 30 s row; a phantom device's 15 s lifts capped to 45.
			in := append(inputs[:4:4], CapInput{DeviceID: "c", Minute: capAt(1), Project: "app", Credited: 15})
			return rows, CapProject(in, "app"), in
		},
		"an input at a minute with no frozen row": func() ([]Minute, []CappedMinute, []CapInput) {
			return rows, capped, append(inputs[:4:4], CapInput{DeviceID: "a", Minute: capAt(9), Project: "lib", Credited: 60})
		},
		"an input's credit above 60": func() ([]Minute, []CappedMinute, []CapInput) {
			in := append(inputs[:2:2], CapInput{DeviceID: "b", Minute: capAt(0), Project: "lib", Credited: 61}, inputs[3])
			return rows, CapProject(in, "app"), in
		},
		"an input's credit below 0": func() ([]Minute, []CappedMinute, []CapInput) {
			in := append(inputs[:2:2], CapInput{DeviceID: "b", Minute: capAt(0), Project: "lib", Credited: -45}, inputs[3])
			return rows, CapProject(in, "app"), in
		},
		"a frozen row twice": func() ([]Minute, []CappedMinute, []CapInput) {
			return append(rows[:3:3], row("a", 1, 30)), capped, inputs
		},
		"an input twice": func() ([]Minute, []CappedMinute, []CapInput) {
			return rows, capped, append(inputs, inputs[0])
		},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			r, c, in := f()
			if err := CheckCap("app", r, c, in); !errors.Is(err, ErrCapMismatch) {
				t.Fatalf("CheckCap = %v, want ErrCapMismatch", err)
			}
		})
	}
}
