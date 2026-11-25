package rubydate

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxLen is Date.parse's default limit: Ruby raises for a longer string
// (in bytes).
const MaxLen = 128

// Date is a calendar date as Ruby's Date reports it: in the Julian
// calendar before 15 October 1582 (Date::ITALY), the Gregorian one from
// then on.
type Date struct {
	Year       int64
	Month, Day int
}

// Parse is Date.parse(s) with Ruby's defaults: two-digit years completed
// (69-99 to 19xx, 0-68 to 20xx), the 128-byte limit, and Date::ITALY.
// today stands in for Date.today, which fills in what s leaves out
// ("June" is 1 June of today's year, "Wed" the Wednesday of today's
// Sunday-to-Saturday week). ok is false where Ruby raises.
func Parse(s string, today time.Time) (d Date, ok bool) {
	if len(s) > MaxLen || !utf8.ValidString(s) {
		return Date{}, false
	}
	f := scan(clean(s))
	return f.date(today.UTC())
}

// clean is date__parse's first step, gsub(/[^-+',.\/:@[:alnum:]\[\]]+/,
// " "), with every non-ASCII letter or digit written as "_".
func clean(s string) string {
	var b strings.Builder
	gap := false
	for i := 0; i < len(s); {
		keep := byte(0)
		if c := s[i]; c < utf8.RuneSelf {
			i++
			if isAlnum(c) || strings.IndexByte(`-+',./:@[]`, c) >= 0 {
				keep = c
			}
		} else {
			r, n := utf8.DecodeRuneInString(s[i:])
			i += n
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				keep = '_'
			}
		}
		if keep != 0 {
			b.WriteByte(keep)
			gap = false
			continue
		}
		if !gap {
			b.WriteByte(' ')
		}
		gap = true
	}
	return b.String()
}

// frags is what date__parse finds: Ruby's hash of date fragments (year,
// mon, mday, yday, cwyear, cweek, cwday, wday, hour, min, sec, and jd,
// wnum0 or wnum1 once completed), with its _comp and _bc flags.
type frags struct {
	v    map[string]int64
	comp bool
	bc   bool
}

func (f *frags) has(keys ...string) bool {
	for _, k := range keys {
		if _, ok := f.v[k]; !ok {
			return false
		}
	}
	return true
}

func (f *frags) set(k string, n int64) { f.v[k] = n }

// scan is date__parse: each step looks for its pattern in what the
// earlier steps left, records what it read and blanks the match out.
func scan(s string) *frags {
	f := &frags{v: map[string]int64{}, comp: true}
	if has(s, alpha) {
		if g, ok := sub(&s, pDay); ok {
			f.set("wday", int64(nameIndex(days, g.at(1))))
		}
	}
	if has(s, digit) {
		if g, ok := sub(&s, pTime); ok {
			f.time(g.at(1))
		}
	}
	// The first of these that matches is the date.
	switch {
	case has(s, alpha|digit) && (f.eu(&s) || f.us(&s)):
	case has(s, digit|dash) && f.iso(&s):
	case has(s, digit|dot) && f.jis(&s):
	case has(s, alpha|digit|dash) && (f.vms11(&s) || f.vms12(&s)):
	case has(s, digit|slash) && f.sla(&s):
	case has(s, digit|dot) && f.dot(&s):
	case has(s, digit) && f.iso2(&s):
	case has(s, digit) && f.year(&s):
	case has(s, alpha) && f.mon(&s):
	case has(s, digit) && f.mday(&s):
	case has(s, digit) && f.ddd(&s):
	}
	if has(s, alpha) {
		if _, ok := sub(&s, pBC); ok {
			f.bc = true
		}
	}
	if has(s, digit) {
		if m := reFrag.FindStringSubmatch(s); m != nil {
			n := num(m[1])
			if f.has("hour") && !f.has("mday") && n >= 1 && n <= 31 {
				f.set("mday", n)
			}
			if f.has("mday") && !f.has("hour") && n >= 0 && n <= 24 {
				f.set("hour", n)
			}
		}
	}
	return f
}

// Character classes date__parse checks the remaining string for before
// it tries a step (check_class).
const (
	alpha = 1 << iota
	digit
	dash
	dot
	slash
)

func has(s string, want int) bool {
	got := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlpha(c):
			got |= alpha
		case isDigit(c):
			got |= digit
		case c == '-':
			got |= dash
		case c == '.':
			got |= dot
		case c == '/':
			got |= slash
		}
	}
	return got&want == want
}

// pattern is one of date_parse.c's regular expressions. Go's regexp has
// no lookbehind, so a pattern is tried anchored at each position in turn
// (the first that matches is Ruby's leftmost match), and start checks
// what Ruby's pattern checks before its first character: \b, or NUMBER's
// (?<!\d).
type pattern struct {
	re    *regexp.Regexp
	start func(s string, i int) bool
}

func pat(expr string, start func(s string, i int) bool) pattern {
	return pattern{re: regexp.MustCompile(`^(?:` + expr + `)`), start: start}
}

// atBoundary is \b at the start of a match.
func atBoundary(s string, i int) bool {
	return (i > 0 && isWord(s[i-1])) != isWord(s[i])
}

// notInNumber is NUMBER's (?<!\d), for a match that starts with its digit.
func notInNumber(s string, i int) bool {
	return !isDigit(s[i]) || i == 0 || !isDigit(s[i-1])
}

// groups is a match: the string it was found in and its submatch indexes.
type groups struct {
	s string
	m []int
}

func (g groups) has(i int) bool { return g.m[2*i] >= 0 }

func (g groups) at(i int) string {
	if !g.has(i) {
		return ""
	}
	return g.s[g.m[2*i]:g.m[2*i+1]]
}

// opt is group i, nil when it took no part (Ruby's nil).
func (g groups) opt(i int) *string {
	if !g.has(i) {
		return nil
	}
	v := g.at(i)
	return &v
}

// sub is date_parse.c's subx: it finds p in *s and replaces the match with
// one space.
func sub(s *string, p pattern) (groups, bool) {
	for i := 0; i < len(*s); i++ {
		if p.start != nil && !p.start(*s, i) {
			continue
		}
		m := p.re.FindStringSubmatchIndex((*s)[i:])
		if m == nil {
			continue
		}
		for k := range m {
			if m[k] >= 0 {
				m[k] += i
			}
		}
		g := groups{s: *s, m: m}
		*s = (*s)[:m[0]] + " " + (*s)[m[1]:]
		return g, true
	}
	return groups{}, false
}

const (
	months = `jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec`
	days   = `sun|mon|tue|wed|thu|fri|sat`
	era    = `c(?:e|\.e\.)|b(?:ce|\.c\.e\.)|a(?:d|\.d\.)|b(?:c|\.c\.)`
)

// date_parse.c's patterns (without TIGHT_PARSER), named after the
// function that uses each; (?i) where Ruby compiles one case-insensitive.
// parse_eu's era is written without its \b and (?!(?<!\.)[a-z]): the year
// that must follow it already ensures both. parse_us's possessive \s*+
// matches what \s* matches here.
var (
	pDay  = pat(`(?i)(`+days+`)[^-/\d\s]*`, atBoundary)
	pTime = pat(`(?i)(\d+\s*(?:(?::\s*\d+(?:\s*:\s*\d+(?:[,.]\d*)?)?|h(?:\s*\d+m?(?:\s*\d+s?)?)?)(?:\s*[ap](?:m\b|\.m\.))?|[ap](?:m\b|\.m\.)))`+
		`(?:\s*((?:gmt|utc?)?[-+]\d+(?:[,.:]\d+(?::\d+)?)?|(?-i:[[:alpha:].\s]+)(?:standard|daylight)\stime\b|(?-i:[[:alpha:]]+)(?:\sdst)?\b))?`, notInNumber)
	reTime2 = regexp.MustCompile(`(?i)^(\d+)h?(?:\s*:?\s*(\d+)m?(?:\s*:?\s*(\d+)(?:[,.](\d+))?s?)?)?(?:\s*([ap])(?:m\b|\.m\.))?`)
	pEU     = pat(`(?i)('?\d+)[^-\d\s]*\s*(`+months+`)[^-\d\s']*(?:\s*(`+era+`)?\s*('?-?\d+(?:(?:st|nd|rd|th)\b)?))?`, notInNumber)
	pUS     = pat(`(?i)(`+months+`)[^-\d\s']*\s*('?\d+)[^-\d\s']*(?:\s*,?\s*(`+era+`)?\s*('?-?\d+))?`, atBoundary)
	pISO    = pat(`('?[-+]?\d+)-(\d+)-('?-?\d+)`, notInNumber)
	pJIS    = pat(`(?i)([mtshr])(\d+)\.(\d+)\.(\d+)`, atBoundary)
	pVMS11  = pat(`(?i)('?-?\d+)-(`+months+`)[^-/.]*-('?-?\d+)`, notInNumber)
	pVMS12  = pat(`(?i)(`+months+`)[^-/.]*-('?-?\d+)(?:-('?-?\d+))?`, atBoundary)
	pSLA    = pat(`('?-?\d+)/\s*('?\d+)(?:\D\s*('?-?\d+))?`, notInNumber)
	pDot    = pat(`('?-?\d+)\.\s*('?\d+)\.\s*('?-?\d+)`, notInNumber)
	pISO21  = pat(`(?i)(\d{2}|\d{4})?-?w(\d{2})(?:-?(\d))?\b`, atBoundary)
	pISO22  = pat(`(?i)-w-(\d)\b`, nil)
	pISO23  = pat(`--(\d{2})?-(\d{2})\b`, nil)
	pISO24  = pat(`--(\d{2})(\d{2})?\b`, nil)
	reISO25 = regexp.MustCompile(`[,.](\d{2}|\d{4})-\d{3}\b`)
	pISO25  = pat(`(\d{2}|\d{4})-(\d{3})\b`, atBoundary)
	reISO26 = regexp.MustCompile(`\d-\d{3}\b`)
	pISO26  = pat(`-(\d{3})\b`, atBoundary)
	pYear   = pat(`'(\d+)\b`, nil)
	pMon    = pat(`(?i)(`+months+`)\S*`, atBoundary)
	pMday   = pat(`(?i)(\d+)(st|nd|rd|th)\b`, notInNumber)
	pDDD    = pat(`(?i)([-+]?)(\d{2,14})(?:\s*t?\s*(\d{2,6})?(?:[,.](\d*))?)?(?:\s*(z\b|[-+]\d{1,4}\b|\[[-+]?\d[^\]]*\]))?`, notInNumber)
	pBC     = pat(`(?i)(bc\b|bce\b|b\.c\.|b\.c\.e\.)`, atBoundary)
	reFrag  = regexp.MustCompile(`^\s*(\d{1,2})\s*$`)
)

// time is parse_time_cb: it reads the hour, minute and second, which
// decide how Date.parse completes a date.
func (f *frags) time(s string) {
	m := reTime2.FindStringSubmatchIndex(s)
	if m == nil {
		return
	}
	g := groups{s: s, m: m}
	f.set("hour", num(g.at(1)))
	if g.has(2) {
		f.set("min", num(g.at(2)))
	}
	if g.has(3) {
		f.set("sec", num(g.at(3)))
	}
}

// isBC reports whether an era group is a BC one (bc, b.c., bce, b.c.e.).
func isBC(era *string) bool { return era != nil && (*era)[0]|0x20 == 'b' }

// monthOf is a month name's number as s3e reads it (Ruby's mon.to_s).
func monthOf(name string) *string {
	m := strconv.Itoa(nameIndex(months, name) + 1)
	return &m
}

func (f *frags) eu(s *string) bool {
	g, ok := sub(s, pEU)
	if ok {
		f.s3e(g.opt(4), monthOf(g.at(2)), g.opt(1), isBC(g.opt(3)))
	}
	return ok
}

func (f *frags) us(s *string) bool {
	g, ok := sub(s, pUS)
	if ok {
		f.s3e(g.opt(4), monthOf(g.at(1)), g.opt(2), isBC(g.opt(3)))
	}
	return ok
}

func (f *frags) iso(s *string) bool {
	g, ok := sub(s, pISO)
	if ok {
		f.s3e(g.opt(1), g.opt(2), g.opt(3), false)
	}
	return ok
}

// jis reads a Japanese era date: M, T, S, H or R, then year.month.day.
func (f *frags) jis(s *string) bool {
	g, ok := sub(s, pJIS)
	if ok {
		base := map[byte]int64{'m': 1867, 't': 1911, 's': 1925, 'h': 1988, 'r': 2018}[g.at(1)[0]|0x20]
		f.set("year", num(g.at(2))+base)
		f.set("mon", num(g.at(3)))
		f.set("mday", num(g.at(4)))
	}
	return ok
}

func (f *frags) vms11(s *string) bool {
	g, ok := sub(s, pVMS11)
	if ok {
		f.s3e(g.opt(3), monthOf(g.at(2)), g.opt(1), false)
	}
	return ok
}

func (f *frags) vms12(s *string) bool {
	g, ok := sub(s, pVMS12)
	if ok {
		f.s3e(g.opt(3), monthOf(g.at(1)), g.opt(2), false)
	}
	return ok
}

func (f *frags) sla(s *string) bool {
	g, ok := sub(s, pSLA)
	if ok {
		f.s3e(g.opt(1), g.opt(2), g.opt(3), false)
	}
	return ok
}

func (f *frags) dot(s *string) bool {
	g, ok := sub(s, pDot)
	if ok {
		f.s3e(g.opt(1), g.opt(2), g.opt(3), false)
	}
	return ok
}

// iso2 reads ISO 8601 week dates, --MM-DD forms and ordinal dates.
func (f *frags) iso2(s *string) bool {
	if g, ok := sub(s, pISO21); ok {
		if g.has(1) {
			f.set("cwyear", num(g.at(1)))
		}
		f.set("cweek", num(g.at(2)))
		if g.has(3) {
			f.set("cwday", num(g.at(3)))
		}
		return true
	}
	if g, ok := sub(s, pISO22); ok {
		f.set("cwday", num(g.at(1)))
		return true
	}
	if g, ok := sub(s, pISO23); ok {
		if g.has(1) {
			f.set("mon", num(g.at(1)))
		}
		f.set("mday", num(g.at(2)))
		return true
	}
	if g, ok := sub(s, pISO24); ok {
		f.set("mon", num(g.at(1)))
		if g.has(2) {
			f.set("mday", num(g.at(2)))
		}
		return true
	}
	if !reISO25.MatchString(*s) {
		if g, ok := sub(s, pISO25); ok {
			f.set("year", num(g.at(1)))
			f.set("yday", num(g.at(2)))
			return true
		}
	}
	if !reISO26.MatchString(*s) {
		if g, ok := sub(s, pISO26); ok {
			f.set("yday", num(g.at(1)))
			return true
		}
	}
	return false
}

func (f *frags) year(s *string) bool {
	g, ok := sub(s, pYear)
	if ok {
		f.set("year", num(g.at(1)))
	}
	return ok
}

func (f *frags) mon(s *string) bool {
	g, ok := sub(s, pMon)
	if ok {
		f.set("mon", int64(nameIndex(months, g.at(1))+1))
	}
	return ok
}

func (f *frags) mday(s *string) bool {
	g, ok := sub(s, pMday)
	if ok {
		f.set("mday", num(g.at(1)))
	}
	return ok
}

// ddd is parse_ddd_cb: a run of 2 to 14 digits read by its length, with
// an optional time after it.
func (f *frags) ddd(s *string) bool {
	g, ok := sub(s, pDDD)
	if !ok {
		return false
	}
	d := g.at(2)
	n := func(from, width int) int64 { return num(d[from : from+width]) }
	year := func(v int64) {
		if g.at(1) == "-" {
			v = -v
		}
		f.set("year", v)
	}
	// A run with a fraction after it but no time is itself a time.
	isTime := !g.has(3) && g.has(4)
	l := len(d)
	switch l {
	case 2:
		if isTime {
			f.set("sec", n(0, 2))
		} else {
			f.set("mday", n(0, 2))
		}
	case 4:
		if isTime {
			f.set("sec", n(2, 2))
			f.set("min", n(0, 2))
		} else {
			f.set("mon", n(0, 2))
			f.set("mday", n(2, 2))
		}
	case 6:
		if isTime {
			f.set("sec", n(4, 2))
			f.set("min", n(2, 2))
			f.set("hour", n(0, 2))
		} else {
			year(n(0, 2))
			f.set("mon", n(2, 2))
			f.set("mday", n(4, 2))
		}
	case 8, 10, 12, 14:
		if isTime {
			f.set("sec", n(l-2, 2))
			f.set("min", n(l-4, 2))
			f.set("hour", n(l-6, 2))
			f.set("mday", n(l-8, 2))
			if l >= 10 {
				f.set("mon", n(l-10, 2))
			}
			if l == 12 {
				year(n(0, 2))
			}
			if l == 14 {
				year(n(0, 4))
				f.comp = false
			}
		} else {
			year(n(0, 4))
			f.set("mon", n(4, 2))
			f.set("mday", n(6, 2))
			if l >= 10 {
				f.set("hour", n(8, 2))
			}
			if l >= 12 {
				f.set("min", n(10, 2))
			}
			if l >= 14 {
				f.set("sec", n(12, 2))
			}
			f.comp = false
		}
	case 3:
		if isTime {
			f.set("sec", n(1, 2))
			f.set("min", n(0, 1))
		} else {
			f.set("yday", n(0, 3))
		}
	case 5:
		if isTime {
			f.set("sec", n(3, 2))
			f.set("min", n(1, 2))
			f.set("hour", n(0, 1))
		} else {
			year(n(0, 2))
			f.set("yday", n(2, 3))
		}
	case 7:
		if isTime {
			f.set("sec", n(5, 2))
			f.set("min", n(3, 2))
			f.set("hour", n(1, 2))
			f.set("mday", n(0, 1))
		} else {
			year(n(0, 4))
			f.set("yday", n(4, 3))
		}
	}
	// The time after the run: hh, hhmm or hhmmss; with a fraction after
	// it, ss, mmss or hhmmss.
	if t := g.at(3); len(t) == 2 || len(t) == 4 || len(t) == 6 {
		names := []string{"hour", "min", "sec"}
		if g.has(4) {
			names = names[3-len(t)/2:]
		}
		for i := 0; i < len(t); i += 2 {
			f.set(names[i/2], num(t[i:i+2]))
		}
	}
	return true
}

// s3e is date_parse.c's s3e: it works out which of three numbers is the
// year, the month and the day (a quoted or more-than-two-digit one is the
// year) and records them.
func (f *frags) s3e(y, m, d *string, bc bool) {
	if y != nil && m != nil && d == nil {
		y, m, d = nil, y, m
	}
	if y == nil {
		if d != nil && len(*d) > 2 {
			y, d = d, nil
		}
		if d != nil && len(*d) > 0 && (*d)[0] == '\'' {
			y, d = d, nil
		}
	}
	if y != nil {
		// A year with something after its digits ("10th") is the day.
		s := *y
		if i := strings.IndexAny(s, "+-0123456789"); i >= 0 {
			bp := i
			if s[i] == '-' || s[i] == '+' {
				i++
			}
			i += digits(s[i:])
			if i < len(s) {
				day := s[bp:i]
				y, d = d, &day
			}
		}
	}
	quotedOrLong := func(v *string) bool { return v != nil && len(*v) > 0 && ((*v)[0] == '\'' || len(*v) > 2) }
	if quotedOrLong(m) {
		y, m, d = m, d, y // year first, not month first
	}
	if quotedOrLong(d) {
		y, d = d, y
	}
	if y != nil {
		s := *y
		if i := strings.IndexAny(s, "+-0123456789"); i >= 0 {
			bp := i
			if s[i] == '-' || s[i] == '+' {
				i++
				f.comp = false
			}
			l := digits(s[i:])
			if l > 2 {
				f.comp = false
			}
			f.set("year", num(s[bp:i+l]))
		}
	}
	if bc {
		f.bc = true
	}
	if m != nil {
		if n, ok := firstNumber(*m); ok {
			f.set("mon", n)
		}
	}
	if d != nil {
		if n, ok := firstNumber(*d); ok {
			f.set("mday", n)
		}
	}
}

// firstNumber reads the first run of digits in s.
func firstNumber(s string) (int64, bool) {
	i := strings.IndexAny(s, "0123456789")
	if i < 0 {
		return 0, false
	}
	return num(s[i : i+digits(s[i:])]), true
}

// maxNum is where num saturates (Ruby's integers don't).
const maxNum = 1_000_000_000_000_000

// num reads an optional sign and digits.
func num(s string) int64 {
	neg := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	var n int64
	for i := 0; i < len(s) && isDigit(s[i]); i++ {
		n = min(n*10+int64(s[i]-'0'), maxNum)
	}
	if neg {
		return -n
	}
	return n
}

func digits(s string) int {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}

// nameIndex is the position in names ("jan|feb|…") of the one s starts
// with, case-insensitively (the patterns only match names in the list).
func nameIndex(names, s string) int {
	for i, n := range strings.Split(names, "|") {
		if len(s) >= len(n) && strings.EqualFold(s[:len(n)], n) {
			return i
		}
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c|0x20) >= 'a' && (c|0x20) <= 'z' }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }
func isWord(c byte) bool  { return isAlnum(c) || c == '_' }
