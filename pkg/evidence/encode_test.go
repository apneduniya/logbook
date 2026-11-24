package evidence

import (
	"bytes"
	"cmp"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The canonical line of every row type, pinned (ADR 0017 point 8; ADR 0019
// keeps it).
func TestLineGolden(t *testing.T) {
	rows := slices.SortedFunc(slices.Values(fixtureBlock()), func(a, b Row) int { return cmp.Compare(a.Seq, b.Seq) })
	var buf bytes.Buffer
	for _, r := range rows {
		line, err := r.Line()
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	golden(t, "block.golden", buf.Bytes())
}

// A line decodes to the row it encodes, and only a canonical line decodes.
func TestDecodeLine(t *testing.T) {
	for _, r := range fixtureBlock() {
		line, err := r.Line()
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeLine(line)
		if err != nil {
			t.Fatalf("seq %d: %v", r.Seq, err)
		}
		again, _ := got.Line()
		if got.Seq != r.Seq || got.Type != r.Type || !got.Time.Equal(r.Time) || !bytes.Equal(again, line) {
			t.Fatalf("seq %d decoded to %+v", r.Seq, got)
		}
	}
	valid := `{"s":2,"t":"2026-11-19T03:29:00Z","type":"repo","v":{"path":"/a","project":"a"}}`
	if _, err := DecodeLine([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"space":          `{"s":2, "t":"2026-11-19T03:29:00Z","type":"repo","v":{"path":"/a","project":"a"}}`,
		"key order":      `{"t":"2026-11-19T03:29:00Z","s":2,"type":"repo","v":{"path":"/a","project":"a"}}`,
		"extra field":    `{"s":2,"t":"2026-11-19T03:29:00Z","type":"repo","v":{"path":"/a","project":"a","x":1}}`,
		"duplicate key":  `{"s":2,"s":3,"t":"2026-11-19T03:29:00Z","type":"repo","v":{"path":"/a","project":"a"}}`,
		"html escaped":   strings.Replace(valid, `"/a"`, `"/`+"\\"+`u003c"`, 1),
		"non-UTC t":      `{"s":2,"t":"2026-11-19T08:59:00+05:30","type":"repo","v":{"path":"/a","project":"a"}}`,
		"t isn't v time": `{"s":3,"t":"2026-11-19T09:00:01Z","type":"activity","v":{"time":"2026-11-19T09:00:00Z","idle_seconds":1.5,"focus_app":"Code","focus_kind":"editor"}}`,
		"unknown type":   `{"s":2,"t":"2026-11-19T03:29:00Z","type":"keystroke","v":{}}`,
		"zero seq":       `{"s":0,"t":"2026-11-19T03:29:00Z","type":"repo","v":{"path":"/a","project":"a"}}`,
		"not json":       `{"s":2`,
		"trailing nl":    valid + "\n",
	} {
		if _, err := DecodeLine([]byte(line)); !errors.Is(err, ErrInvalidLine) {
			t.Errorf("%s: err = %v, want ErrInvalidLine", name, err)
		}
	}
}

func TestLineRejectsInvalidRows(t *testing.T) {
	for name, r := range map[string]Row{
		"zero seq":     {Seq: 0, Type: TypeRepo, Value: Repo{}},
		"negative seq": {Seq: -1, Type: TypeRepo, Value: Repo{}},
		"unknown type": {Seq: 1, Type: "keystroke", Value: Repo{}},
	} {
		if _, err := r.Line(); !errors.Is(err, ErrInvalidRow) {
			t.Errorf("%s: err = %v, want ErrInvalidRow", name, err)
		}
	}
}

func TestCounts(t *testing.T) {
	c := Counts(fixtureBlock())
	want := map[string]int{TypeActivity: 4, TypeActiveTime: 1, TypeAI: 2, TypeGit: 1, TypeHeartbeat: 2, TypeRepo: 2}
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

func TestDayBounds(t *testing.T) {
	start, end, err := DayBounds("2026-11-19")
	if err != nil || start.Format("2006-01-02T15:04") != "2026-11-19T00:00" || end.Sub(start).Hours() != 24 {
		t.Fatalf("DayBounds = %v, %v, %v", start, end, err)
	}
	if _, _, err := DayBounds("19-11-2026"); !errors.Is(err, ErrInvalidDay) || !strings.Contains(err.Error(), "19-11") {
		t.Fatalf("bad day: %v", err)
	}
}
