package stats

import (
	"fmt"
	"time"
)

// dateLayout is the wire format for calendar dates.
const dateLayout = "2006-01-02"

// Date is a calendar date with no time zone attached. The zero value is not
// a valid date; build one with ParseDate or DateOf.
type Date struct {
	t time.Time // always 00:00 UTC on the date
}

// ParseDate parses a YYYY-MM-DD string.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("parsing date %q: %w", s, err)
	}
	return Date{t: t}, nil
}

// DateOf returns the local calendar date of t in loc.
func DateOf(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{t: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
}

// String formats the date as YYYY-MM-DD.
func (d Date) String() string { return d.t.Format(dateLayout) }

// AddDays returns the date n days later (or earlier when n is negative).
func (d Date) AddDays(n int) Date { return Date{t: d.t.AddDate(0, 0, n)} }

// Before reports whether d is earlier than o.
func (d Date) Before(o Date) bool { return d.t.Before(o.t) }

// Equal reports whether d and o are the same date.
func (d Date) Equal(o Date) bool { return d.t.Equal(o.t) }

// IsZero reports whether d is the zero Date.
func (d Date) IsZero() bool { return d.t.IsZero() }

// Start returns the first instant of d in loc. On days where local midnight
// does not exist (a DST jump at 00:00) it is the first instant that does.
func (d Date) Start(loc *time.Location) time.Time {
	y, m, day := d.t.Date()
	return time.Date(y, m, day, 0, 0, 0, 0, loc)
}

// DaysUntil returns the number of days from d to o (negative if o is earlier).
func (d Date) DaysUntil(o Date) int {
	return int(o.t.Sub(d.t).Hours() / 24)
}
