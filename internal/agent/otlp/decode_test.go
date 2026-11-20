package otlp

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func minute(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeDeltaFixture(t *testing.T) {
	d, err := Decode(fixture(t, "delta.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Point{
		{SessionID: "s1", Type: "user", Start: minute(-2), Time: minute(-1), Value: 21.5},
		{SessionID: "s1", Type: "cli", Start: minute(-2), Time: minute(-1), Value: 38.25},
		{SessionID: "s2", Type: "user", Repository: "my-app", Start: minute(-2), Time: minute(-1), Value: 12},
		// No session.id on the datapoint: the resource's is used.
		{SessionID: "resource-session", Type: "user", Start: minute(-2), Time: minute(-1), Value: 3},
	}
	if d.Rejected != 1 { // type "idle"
		t.Fatalf("rejected = %d, want 1", d.Rejected)
	}
	if len(d.Points) != len(want) {
		t.Fatalf("points = %+v", d.Points)
	}
	for i := range want {
		if d.Points[i] != want[i] {
			t.Errorf("point %d = %+v, want %+v", i, d.Points[i], want[i])
		}
	}
}

// Only the three kept attributes ever leave the decoder.
func TestDecodeKeepsNoIdentity(t *testing.T) {
	d, err := Decode(fixture(t, "delta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range d.Points {
		for _, s := range []string{p.SessionID, p.Type, p.Repository} {
			if strings.Contains(s, "SENTINEL") {
				t.Fatalf("identity attribute kept: %+v", p)
			}
		}
	}
}

func TestDecodeCases(t *testing.T) {
	point := func(temporality, value, attrs string) string {
		return `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"claude_code.active_time.total","sum":{` +
			`"aggregationTemporality":` + temporality + `,"dataPoints":[{"attributes":[` + attrs + `],` +
			`"startTimeUnixNano":"1795168680000000000","timeUnixNano":"1795168740000000000",` + value + `}]}}]}]}]}`
	}
	sess := `{"key":"session.id","value":{"stringValue":"s"}},{"key":"type","value":{"stringValue":"cli"}}`
	tests := []struct {
		name       string
		body       string
		points     int
		rejected   int
		cumulative bool
		value      float64
		err        bool
	}{
		{"delta int", point("1", `"asInt":"7"`, sess), 1, 0, false, 7, false},
		{"cumulative by name", point(`"AGGREGATION_TEMPORALITY_CUMULATIVE"`, `"asDouble":7.5`, sess), 1, 0, true, 7.5, false},
		{"delta by name", point(`"AGGREGATION_TEMPORALITY_DELTA"`, `"asDouble":1`, sess), 1, 0, false, 1, false},
		{"unspecified temporality", point("0", `"asDouble":1`, sess), 0, 1, false, 0, false},
		{"no value", point("1", `"exemplars":[]`, sess), 0, 1, false, 0, false},
		{"negative", point("1", `"asDouble":-1`, sess), 0, 1, false, 0, false},
		{"NaN", point("1", `"asDouble":"NaN"`, sess), 0, 1, false, 0, false},
		{"no session", point("1", `"asDouble":1`, `{"key":"type","value":{"stringValue":"cli"}}`), 0, 1, false, 0, false},
		{"control chars in session", point("1", `"asDouble":1`,
			`{"key":"session.id","value":{"stringValue":"a\nb"}},{"key":"type","value":{"stringValue":"cli"}}`), 0, 1, false, 0, false},
		{"session too long", point("1", `"asDouble":1`,
			`{"key":"session.id","value":{"stringValue":"`+strings.Repeat("x", 300)+`"}},{"key":"type","value":{"stringValue":"cli"}}`), 0, 1, false, 0, false},
		{"session not a string", point("1", `"asDouble":1`,
			`{"key":"session.id","value":{"intValue":"5"}},{"key":"type","value":{"stringValue":"cli"}}`), 0, 1, false, 0, false},
		{"other metric", strings.Replace(point("1", `"asDouble":1`, sess), "active_time", "cost", 1), 0, 0, false, 0, false},
		{"gauge ignored", strings.Replace(point("1", `"asDouble":1`, sess), `"sum"`, `"gauge"`, 1), 0, 0, false, 0, false},
		{"empty", `{}`, 0, 0, false, 0, false},
		{"not json", `{"resourceMetrics":`, 0, 0, false, 0, true},
		{"wrong shape", `{"resourceMetrics":{}}`, 0, 0, false, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Decode([]byte(tc.body))
			if tc.err {
				if !errors.Is(err, ErrBadRequest) {
					t.Fatalf("err = %v, want ErrBadRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Points) != tc.points || d.Rejected != tc.rejected {
				t.Fatalf("points=%+v rejected=%d", d.Points, d.Rejected)
			}
			if tc.points == 1 && (d.Points[0].Cumulative != tc.cumulative || d.Points[0].Value != tc.value) {
				t.Fatalf("point = %+v", d.Points[0])
			}
		})
	}
}

// Plain-number timestamps (not the OTLP JSON string form) are read too.
func TestDecodeNumericTimestamps(t *testing.T) {
	body := `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"claude_code.active_time.total","sum":{` +
		`"aggregationTemporality":1,"dataPoints":[{"attributes":[{"key":"session.id","value":{"stringValue":"s"}},` +
		`{"key":"type","value":{"stringValue":"user"}}],"startTimeUnixNano":1795168680000000000,"timeUnixNano":1795168740000000000,"asDouble":2}]}}]}]}]}`
	d, err := Decode([]byte(body))
	if err != nil || len(d.Points) != 1 || !d.Points[0].Time.Equal(minute(-1)) {
		t.Fatalf("Decode = %+v, %v", d, err)
	}
}

func TestDecodeTooManyPoints(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"claude_code.active_time.total","sum":{"aggregationTemporality":1,"dataPoints":[`)
	for i := range maxPoints + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"timeUnixNano":"` + strconv.Itoa(i+1) + `","asDouble":1}`)
	}
	b.WriteString(`]}}]}]}]}`)
	if _, err := Decode([]byte(b.String())); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
}
