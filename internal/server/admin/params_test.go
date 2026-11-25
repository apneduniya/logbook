package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Rails merges the body under the query (request_parameters.merge(
// query_parameters)): a key sent in both is the query's.
func TestReadParamsQueryWins(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, contentType, body string
	}{
		{"JSON", "application/json", `{"email": "b@example.com", "query": "from body"}`},
		{"form", "application/x-www-form-urlencoded", `email=b%40example.com&query=from+body`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/admin/v1/user/get_user_by_email?email=a@example.com", strings.NewReader(c.body))
			r.Header.Set("Content-Type", c.contentType)
			p, err := readParams(r)
			if err != nil {
				t.Fatal(err)
			}
			if p["email"] != "a@example.com" || p["query"] != "from body" {
				t.Fatalf("params = %v", p)
			}
		})
	}
}

// include_ai is cast as ActiveModel::Type::Boolean casts it, whose false
// values are case-sensitive: "False" and "Off" are true.
func TestBoolParamIsRailsBoolean(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"": false, "0": false, "f": false, "F": false, "false": false, "FALSE": false, "off": false, "OFF": false,
		"False": true, "Off": true, "fALSE": true, "oFF": true, "1": true, "true": true, "no": true, " ": true, "0 ": true,
	} {
		if got := (params{"include_ai": v}).boolParam("include_ai"); got != want {
			t.Errorf("include_ai=%q = %v, want %v", v, got, want)
		}
	}
	if (params{}).boolParam("include_ai") {
		t.Error("a missing include_ai is true")
	}
}

// parseTS reads a date as Date.parse does (rubydate checks the parser
// against Ruby itself), clamps the years Ruby allows beyond 1 to 9999,
// and still reads digits as Unix seconds.
func TestParseTSReadsDatesAsDateParse(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	jun10 := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	invalid := time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC) // never a result
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"2026-06-10", jun10},
		{" 2026-06-10\n", jun10},
		{"2026-06-10T12:00:00Z", jun10},
		{"Wed, 10 Jun 2026 00:00:00 GMT", jun10}, // JavaScript's toUTCString
		{"Wed Jun 10 2026 00:00:00 GMT+0000 (Coordinated Universal Time)", jun10},
		{"June 10 2026", jun10},
		{"June 10, 2026", jun10},
		{"10 Jun 2026", jun10},
		{"10/06/2026", jun10}, // day first, as Date.parse reads it
		{"10.06.2026", jun10},
		{"10-Jun-2026", jun10},
		{"abc2026-06-10", jun10},
		{"2026-W24-3", jun10},
		{"2026-161", jun10},
		{"Jun 2026", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"June", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"10th", time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)},
		{"Wed", time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)},
		{"10000-01-01", maxDay},
		{"-2026-06-10", minDay},
		{"1781085600", time.Unix(1781085600, 0).UTC()},
		{"1781085600000", time.Unix(1781085600, 0).UTC()},
		{"2026-06", invalid}, // Date.parse reads month 20, day 26
		{"2024-02-30", invalid},
		{"yesterday", invalid},
		{"soon", invalid},
		{strings.Repeat("x", 119) + "2026-06-10", invalid}, // over Date.parse's 128-byte limit
		{"253402300800", invalid},
	} {
		got, ok := parseTS(c.in, false, today)
		if !ok {
			got = invalid
		}
		if !got.Equal(c.want) {
			t.Errorf("parseTS(%q) = %v %v, want %v", c.in, got, ok, c.want)
		}
	}
	if end, _ := parseTS("Wed, 10 Jun 2026 00:00:00 GMT", true, today); !end.Equal(jun10.Add(24*time.Hour - time.Second)) {
		t.Errorf("end of 10 Jun = %v", end)
	}
}
