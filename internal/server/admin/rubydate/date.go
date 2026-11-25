package rubydate

import "time"

// date is what Date.parse does with the fragments (d_new_by_frags): BC
// and two-digit years first, then a year, month and day as they are, or
// else the fragments completed from today and the first set of them that
// makes a valid date.
func (f *frags) date(today time.Time) (Date, bool) {
	for _, k := range []string{"cwyear", "year"} {
		v, ok := f.v[k]
		if !ok {
			continue
		}
		if f.bc {
			v = 1 - v
		}
		if f.comp && v >= 0 && v <= 99 {
			if v >= 69 {
				v += 1900
			} else {
				v += 2000
			}
		}
		f.v[k] = v
	}
	if !f.has("yday") && f.has("year", "mon", "mday") {
		jd, ok := validCivil(f.v["year"], f.v["mon"], f.v["mday"])
		return civilIf(jd, ok)
	}
	f.complete(today)
	return civilIf(f.valid())
}

func civilIf(jd int64, ok bool) (Date, bool) {
	if !ok {
		return Date{}, false
	}
	y, m, d := jdToCivil(jd)
	return Date{Year: y, Month: int(m), Day: int(d)}, true
}

// completions is rt_complete_frags' table: the set of fragments with the
// most present is the kind of date they are, and what it lacks is filled
// in. Sets without a kind fill nothing.
var completions = []struct {
	kind string
	keys []string
}{
	{"time", []string{"hour", "min", "sec"}},
	{"", []string{"jd"}},
	{"ordinal", []string{"year", "yday", "hour", "min", "sec"}},
	{"civil", []string{"year", "mon", "mday", "hour", "min", "sec"}},
	{"commercial", []string{"cwyear", "cweek", "cwday", "hour", "min", "sec"}},
	{"wday", []string{"wday", "hour", "min", "sec"}},
	{"wnum0", []string{"year", "wnum0", "wday", "hour", "min", "sec"}},
	{"wnum1", []string{"year", "wnum1", "wday", "hour", "min", "sec"}},
	{"", []string{"cwyear", "cweek", "wday", "hour", "min", "sec"}},
	{"", []string{"year", "wnum0", "cwday", "hour", "min", "sec"}},
	{"", []string{"year", "wnum1", "cwday", "hour", "min", "sec"}},
}

// complete is rt_complete_frags for Date: the leading fields the kind
// lacks come from today, the trailing ones are their first value.
func (f *frags) complete(today time.Time) {
	best, most := -1, 0
	for i, c := range completions {
		n := 0
		for _, k := range c.keys {
			if f.has(k) {
				n++
			}
		}
		if n > most {
			best, most = i, n
		}
	}
	if best < 0 || completions[best].kind == "" || len(completions[best].keys) == most {
		return
	}
	c := completions[best]
	now := todayFields(today)
	fill := func(defaults map[string]int64) {
		for _, k := range c.keys {
			if f.has(k) {
				break
			}
			f.set(k, now[k])
		}
		for k, v := range defaults {
			if !f.has(k) {
				f.set(k, v)
			}
		}
	}
	switch c.kind {
	case "ordinal":
		if !f.has("year") {
			f.set("year", now["year"])
		}
		if !f.has("yday") {
			f.set("yday", 1)
		}
	case "civil":
		fill(map[string]int64{"mon": 1, "mday": 1})
	case "commercial":
		fill(map[string]int64{"cweek": 1, "cwday": 1})
	case "wday":
		f.set("jd", now["jd"]-now["wday"]+f.v["wday"])
	case "wnum0":
		fill(map[string]int64{"wnum0": 0, "wday": 0})
	case "wnum1":
		fill(map[string]int64{"wnum1": 0, "wday": 1})
	}
	// "time" fills a day only for DateTime, never for Date.
}

// todayFields are Date.today's fields that completion may read.
func todayFields(today time.Time) map[string]int64 {
	y, m, d := today.Date()
	jd, _ := validCivil(int64(y), int64(m), int64(d))
	cy, cw, cd := jdToCommercial(jd)
	_, w0, _ := jdToWeeknum(jd, 0)
	return map[string]int64{
		"year": int64(y), "mon": int64(m), "mday": int64(d), "jd": jd, "wday": wday(jd),
		"cwyear": cy, "cweek": cw, "cwday": cd, "wnum0": w0,
	}
}

// valid is rt__valid_date_frags_p: the first of the fragments' readings
// (Julian day, ordinal, civil, commercial, week numbers) that is a date.
func (f *frags) valid() (int64, bool) {
	v := f.v
	if jd, ok := v["jd"]; ok {
		return jd, true
	}
	if f.has("yday", "year") {
		if jd, ok := validOrdinal(v["year"], v["yday"]); ok {
			return jd, true
		}
	}
	if f.has("mday", "mon", "year") {
		if jd, ok := validCivil(v["year"], v["mon"], v["mday"]); ok {
			return jd, true
		}
	}
	// Commercial: cwday, else wday with Sunday as 7.
	wd, hasWD := v["cwday"]
	if !hasWD {
		if wd, hasWD = v["wday"]; hasWD && wd == 0 {
			wd = 7
		}
	}
	if hasWD && f.has("cweek", "cwyear") {
		if jd, ok := validCommercial(v["cwyear"], v["cweek"], wd); ok {
			return jd, true
		}
	}
	// Week numbers: wday, else cwday with Sunday as 0.
	wd, hasWD = v["wday"]
	if !hasWD {
		if wd, hasWD = v["cwday"]; hasWD && wd == 7 {
			wd = 0
		}
	}
	if hasWD && f.has("wnum0", "year") {
		if jd, ok := validWeeknum(v["year"], v["wnum0"], wd, 0); ok {
			return jd, true
		}
	}
	wd, hasWD = v["wday"]
	if !hasWD {
		wd, hasWD = v["cwday"]
	}
	if hasWD && f.has("wnum1", "year") {
		if jd, ok := validWeeknum(v["year"], v["wnum1"], mod7(wd-1), 1); ok {
			return jd, true
		}
	}
	return 0, false
}

// The calendar, as date_core.c computes it with Date::ITALY: dates from
// Julian day italy on are Gregorian, earlier ones Julian.
const italy = 2299161 // 1582-10-15

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// mod7 is a modulo 7 that is never negative.
func mod7(a int64) int64 { return a - 7*floorDiv(a, 7) }

// civilToJD is c_civil_to_jd: the Gregorian day number, or the Julian one
// when the Gregorian one is before the reform.
func civilToJD(y, m, d int64) int64 {
	a := floorDiv(14-m, 12)
	yy := y + 4800 - a
	mm := m + 12*a - 3
	days := d + floorDiv(153*mm+2, 5) + 365*yy + floorDiv(yy, 4)
	if jd := days - floorDiv(yy, 100) + floorDiv(yy, 400) - 32045; jd >= italy {
		return jd
	}
	return days - 32083
}

// jdToCivil is c_jd_to_civil.
func jdToCivil(jd int64) (y, m, d int64) {
	var b, c int64
	if jd >= italy {
		a := jd + 32044
		b = floorDiv(4*a+3, 146097)
		c = a - floorDiv(146097*b, 4)
	} else {
		c = jd + 32082
	}
	dd := floorDiv(4*c+3, 1461)
	e := c - floorDiv(1461*dd, 4)
	mm := floorDiv(5*e+2, 153)
	d = e - floorDiv(153*mm+2, 5) + 1
	m = mm + 3 - 12*floorDiv(mm, 10)
	y = 100*b + dd - 4800 + floorDiv(mm, 10)
	return y, m, d
}

// validCivil is c_valid_civil_p for the months and days a parse yields
// (never negative): the date's day number, if it is a date.
func validCivil(y, m, d int64) (int64, bool) {
	if m < 1 || m > 12 || d < 1 || d > 31 {
		return 0, false
	}
	jd := civilToJD(y, m, d)
	ry, rm, rd := jdToCivil(jd)
	return jd, ry == y && rm == m && rd == d
}

// firstDay is c_find_fdoy: 1 January of y.
func firstDay(y int64) int64 { return civilToJD(y, 1, 1) }

// validOrdinal is c_valid_ordinal_p for days of the year a parse yields.
func validOrdinal(y, yday int64) (int64, bool) {
	jd := firstDay(y) + yday - 1
	ry, _, _ := jdToCivil(jd)
	return jd, ry == y && jd-firstDay(ry)+1 == yday
}

func commercialToJD(y, w, d int64) int64 {
	j := firstDay(y) + 3
	return j - mod7(j) + 7*(w-1) + (d - 1)
}

func jdToCommercial(jd int64) (y, w, d int64) {
	a, _, _ := jdToCivil(jd - 3)
	first := commercialToJD(a+1, 1, 1)
	y = a + 1
	if jd < first {
		first = commercialToJD(a, 1, 1)
		y = a
	}
	w = 1 + floorDiv(jd-first, 7)
	d = mod7(jd + 1)
	if d == 0 {
		d = 7
	}
	return y, w, d
}

// validCommercial is c_valid_commercial_p for the weeks and days a parse
// yields.
func validCommercial(y, w, d int64) (int64, bool) {
	jd := commercialToJD(y, w, d)
	ry, rw, rd := jdToCommercial(jd)
	return jd, ry == y && rw == w && rd == d
}

func weeknumToJD(y, w, d, f int64) int64 {
	j := firstDay(y) + 6
	return j - mod7(j-f+1) - 7 + 7*w + d
}

func jdToWeeknum(jd, f int64) (y, w, d int64) {
	y, _, _ = jdToCivil(jd)
	j := firstDay(y) + 6
	n := jd - (j - mod7(j-f+1)) + 7
	return y, floorDiv(n, 7), mod7(n)
}

// validWeeknum is c_valid_weeknum_p for the weeks and days a parse yields.
func validWeeknum(y, w, d, f int64) (int64, bool) {
	jd := weeknumToJD(y, w, d, f)
	ry, rw, rd := jdToWeeknum(jd, f)
	return jd, ry == y && rw == w && rd == d
}

// wday is c_jd_to_wday: 0 for Sunday.
func wday(jd int64) int64 { return mod7(jd + 1) }
