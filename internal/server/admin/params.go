package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxBody caps a POST body (the two POST calls take one short field).
const maxBody = 64 << 10

// params are a request's parameters as Rails merges them
// (ActionDispatch::Http::Parameters#parameters is
// request_parameters.merge(query_parameters)): the body (JSON or a form),
// then the query over it, so the query wins.
type params map[string]string

// readParams reads, for a body, a JSON object or a form, then the query.
// JSON values that aren't strings are kept as their JSON text.
func readParams(r *http.Request) (params, error) {
	p := params{}
	if r.Body != nil && r.Method != http.MethodGet {
		if err := readBody(r, p); err != nil {
			return nil, err
		}
	}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			p[k] = vs[len(vs)-1] // Rails keeps the last of repeated keys
		}
	}
	return p, nil
}

// readBody adds a body's parameters to p.
func readBody(r *http.Request, p params) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxBody {
		return errBodyTooLarge
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return errBadJSON
		}
		for k, raw := range m {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				p[k] = s
			} else if string(raw) != "null" {
				p[k] = string(raw)
			}
		}
	default:
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return errBadForm
		}
		for k, vs := range form {
			if len(vs) > 0 {
				p[k] = vs[len(vs)-1]
			}
		}
	}
	return nil
}

var (
	errBodyTooLarge = errors.New("request body too large")
	errBadJSON      = errors.New("invalid JSON body")
	errBadForm      = errors.New("invalid form body")
)

// has reports whether the parameter was sent at all.
func (p params) has(name string) bool {
	_, ok := p[name]
	return ok
}

// present is Rails' present?: sent and not only whitespace.
func (p params) present(name string) bool { return strings.TrimSpace(p[name]) != "" }

// toI is Ruby's String#to_i: optional leading whitespace and sign, then
// digits; 0 without any. Values beyond int64 saturate.
func toI(s string) int64 {
	n, _ := leadingInt(s)
	return n
}

// leadingInt parses Ruby's leading integer of s; ok is false when s has no
// leading digits.
func leadingInt(s string) (n int64, ok bool) {
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	neg := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(s[:end], 10, 64)
	if err != nil {
		v = math.MaxInt64
	}
	if neg {
		v = -v
	}
	return v, true
}

// clamp is Ruby's Comparable#clamp.
func clamp(n, lo, hi int64) int64 { return max(lo, min(n, hi)) }

// intParam is (params[name] || def).to_i.clamp(lo, hi).
func (p params) intParam(name string, def, lo, hi int64) int64 {
	if !p.has(name) {
		return clamp(def, lo, hi)
	}
	return clamp(toI(p[name]), lo, hi)
}

// boolParam is ActiveModel::Type::Boolean's cast: false for a missing or
// empty value and for its FALSE_VALUES, which are case-sensitive ("0",
// "f", "F", "false", "FALSE", "off", "OFF"); true otherwise, "False" and
// "Off" included.
func (p params) boolParam(name string) bool {
	v, ok := p[name]
	if !ok || v == "" {
		return false
	}
	switch v {
	case "0", "f", "F", "false", "FALSE", "off", "OFF":
		return false
	}
	return true
}

// railsID casts a user id parameter as ActiveRecord's integer type does
// for User.find: a string starting with an optional sign and a digit
// gives its leading integer, anything else no id (ok false, so the user
// isn't found).
func railsID(s string) (int64, bool) {
	return leadingInt(s)
}

// maxTS is the largest timestamp Hackatime accepts (9999-12-31T23:59:59Z).
const maxTS = 253_402_300_799

// datePattern finds the date at the start of a value, which is what
// Ruby's Date.parse reads of an ISO date or date-time.
var datePattern = regexp.MustCompile(`^(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})`)

// parseTS is Hackatime's DateParsing#parse_ts: digits are Unix seconds
// (milliseconds from 10^12 on) between 0 and maxTS; anything else is read
// as a date, giving the start of that UTC day, or its last second for an
// end boundary. ok is false when the value is neither.
func parseTS(raw string, end bool) (t time.Time, ok bool) {
	v := strings.TrimSpace(raw)
	if isDigits(v) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		if n >= 1_000_000_000_000 {
			n /= 1000
		}
		if n < 0 || n > maxTS {
			return time.Time{}, false
		}
		return time.Unix(n, 0).UTC(), true
	}
	m := datePattern.FindStringSubmatch(v)
	if m == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1]) // the pattern matched digits
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	day := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if day.Year() != y || int(day.Month()) != mo || day.Day() != d {
		return time.Time{}, false // Date.parse rejects 2024-02-30
	}
	if end {
		return day.Add(24*time.Hour - time.Second), true
	}
	return day, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// timeRange is a closed range of heartbeat times; a nil bound is open.
type timeRange struct {
	From, To *time.Time
}

// applyTimeRange is DateParsing#apply_time_range: start_date and end_date
// bound the range when present. It returns the name of the parameter that
// didn't parse.
func (p params) applyTimeRange() (timeRange, string) {
	var rg timeRange
	if p.present("start_date") {
		t, ok := parseTS(p["start_date"], false)
		if !ok {
			return rg, "start_date"
		}
		rg.From = &t
	}
	if p.present("end_date") {
		t, ok := parseTS(p["end_date"], true)
		if !ok {
			return rg, "end_date"
		}
		rg.To = &t
	}
	return rg, ""
}

// defaultTimeRange is DateParsing#parse_default_time_range: start_date
// defaults to ten years ago, end_date to the end of today (UTC).
func (p params) defaultTimeRange(now time.Time) (timeRange, string) {
	from := time.Unix(now.AddDate(-10, 0, 0).Unix(), 0).UTC()
	y, m, d := now.UTC().Date()
	to := time.Date(y, m, d, 23, 59, 59, 0, time.UTC)
	if p.present("start_date") {
		t, ok := parseTS(p["start_date"], false)
		if !ok {
			return timeRange{}, "start_date"
		}
		from = t
	}
	if p.present("end_date") {
		t, ok := parseTS(p["end_date"], true)
		if !ok {
			return timeRange{}, "end_date"
		}
		to = t
	}
	return timeRange{From: &from, To: &to}, ""
}
