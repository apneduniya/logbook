package rubydate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Parse gives Ruby's answer for every input in testdata/date_parse.jsonl:
// hand-picked forms (ISO, RFC 2822, month names, slashes, dots, week and
// ordinal dates, Japanese eras, digit runs, BC, two-digit years, the
// calendar reform, the 128-byte limit, non-ASCII) and 2,500 seeded random
// mixes of date tokens. testdata/date_parse.rb wrote it with Ruby's
// Date.parse (TZ=UTC, date gem 2.0.3; Hackatime runs 3.5.1, whose parser this
// ports);
// its first line is the Date.today it ran on.
func TestParseMatchesRuby(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/date_parse.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	if !sc.Scan() {
		t.Fatal("empty testdata")
	}
	var head struct {
		Today string `json:"today"`
	}
	if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
		t.Fatal(err)
	}
	today, err := time.Parse(time.DateOnly, head.Today)
	if err != nil {
		t.Fatal(err)
	}
	n, fails := 0, 0
	for sc.Scan() {
		var c struct {
			In  string  `json:"in"`
			Out *string `json:"out"`
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		n++
		want := "invalid"
		if c.Out != nil {
			want = *c.Out
		}
		got := "invalid"
		if d, ok := Parse(c.In, today.Add(15*time.Hour)); ok {
			got = fmt.Sprintf("%d-%02d-%02d", d.Year, d.Month, d.Day)
		}
		// Years past maxNum saturate here; Ruby's don't.
		saturated := got != "invalid" && want != "invalid" && strings.IndexByte(strings.TrimPrefix(want, "-"), '-') > 15
		if got != want && !saturated {
			fails++
			if fails <= 40 {
				t.Errorf("Parse(%q) = %s, Ruby: %s", c.In, got, want)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 2500 {
		t.Fatalf("only %d cases", n)
	}
	if fails > 0 {
		t.Fatalf("%d of %d differ from Ruby", fails, n)
	}
}

// The calendar functions agree with each other across the reform and far
// from it: every day number maps to a date that maps back to it.
func TestCalendarRoundTrips(t *testing.T) {
	t.Parallel()
	for _, start := range []int64{-5_000_000, -1000, 0, italy - 400, 2_440_588 - 400, 5_000_000} {
		for jd := start; jd < start+800; jd++ {
			y, m, d := jdToCivil(jd)
			if back, ok := validCivil(y, m, d); !ok || back != jd {
				t.Fatalf("jd %d -> %d-%d-%d -> %d %v", jd, y, m, d, back, ok)
			}
			cy, cw, cd := jdToCommercial(jd)
			if back, ok := validCommercial(cy, cw, cd); !ok || back != jd {
				t.Fatalf("jd %d -> commercial %d-W%d-%d -> %d %v", jd, cy, cw, cd, back, ok)
			}
		}
	}
	// The reform skips 5 to 14 October 1582; 1500 is a Julian leap year.
	for _, c := range []struct {
		y, m, d int64
		ok      bool
	}{{1582, 10, 4, true}, {1582, 10, 5, false}, {1582, 10, 14, false}, {1582, 10, 15, true}, {1500, 2, 29, true}, {1900, 2, 29, false}} {
		if _, ok := validCivil(c.y, c.m, c.d); ok != c.ok {
			t.Errorf("validCivil(%d, %d, %d) = %v", c.y, c.m, c.d, ok)
		}
	}
}

// Strings over Ruby's 128-byte limit are refused whatever they hold.
func TestParseLimit(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	if _, ok := Parse(strings.Repeat(" ", MaxLen-10)+"2026-06-10", today); !ok {
		t.Fatal("128 bytes refused")
	}
	if _, ok := Parse("2026-06-10"+strings.Repeat("x", MaxLen-9), today); ok {
		t.Fatal("129 bytes accepted")
	}
	if _, ok := Parse("2026-06-10\xff", today); ok {
		t.Fatal("invalid UTF-8 accepted")
	}
}
