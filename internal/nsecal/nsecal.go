// Package nsecal answers whether the NSE equity segment trades on a date. It
// reads deploy/nse-holidays.yaml, which the market-hours workflow uses to
// decide whether to start the VM.
package nsecal

import (
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

const dateLayout = "2006-01-02"

// Entry is one dated calendar line.
type Entry struct {
	Date     string `yaml:"date"`
	Name     string `yaml:"name"`
	Verified bool   `yaml:"verified"`
}

// Calendar is the parsed holiday file.
type Calendar struct {
	YearsComplete []int   `yaml:"years_complete"`
	Holidays      []Entry `yaml:"holidays"`
	ExtraSessions []Entry `yaml:"extra_sessions"`

	holidays map[string]Entry
	extra    map[string]Entry
}

// Parse reads and validates a calendar. Unknown fields, malformed or repeated
// dates, and weekend holidays or weekday extra sessions are errors, because
// they would silently do nothing.
func Parse(r io.Reader) (*Calendar, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var c Calendar
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse holiday calendar: %w", err)
	}
	var err error
	if c.holidays, err = index(c.Holidays, "holidays", false); err != nil {
		return nil, err
	}
	if c.extra, err = index(c.ExtraSessions, "extra_sessions", true); err != nil {
		return nil, err
	}
	for d := range c.extra {
		if _, ok := c.holidays[d]; ok {
			return nil, fmt.Errorf("%s is both a holiday and an extra session", d)
		}
	}
	return &c, nil
}

func index(entries []Entry, field string, weekend bool) (map[string]Entry, error) {
	m := make(map[string]Entry, len(entries))
	for _, e := range entries {
		t, err := time.Parse(dateLayout, e.Date)
		if err != nil {
			return nil, fmt.Errorf("%s: bad date %q (want YYYY-MM-DD)", field, e.Date)
		}
		if isWeekend := t.Weekday() == time.Saturday || t.Weekday() == time.Sunday; isWeekend != weekend {
			if weekend {
				return nil, fmt.Errorf("%s: %s is a %s, not a weekend", field, e.Date, t.Weekday())
			}
			return nil, fmt.Errorf("%s: %s is a %s; weekends are always closed", field, e.Date, t.Weekday())
		}
		if _, dup := m[e.Date]; dup {
			return nil, fmt.Errorf("%s: %s is listed twice", field, e.Date)
		}
		m[e.Date] = e
	}
	return m, nil
}

// Result is the answer for one date.
type Result struct {
	Open    bool
	Reason  string // why the date is open or closed
	Warning string // set when the date's year is not marked complete
}

// Check reports whether the exchange trades on date, given as YYYY-MM-DD.
func (c *Calendar) Check(date string) (Result, error) {
	t, err := time.Parse(dateLayout, date)
	if err != nil {
		return Result{}, fmt.Errorf("bad date %q (want YYYY-MM-DD)", date)
	}
	var res Result
	switch e, isHoliday := c.holidays[date]; {
	case isHoliday:
		res = Result{Reason: "NSE holiday: " + e.Name}
	case t.Weekday() == time.Saturday || t.Weekday() == time.Sunday:
		if e, ok := c.extra[date]; ok {
			res = Result{Open: true, Reason: "extra session: " + e.Name}
		} else {
			res = Result{Reason: "weekend"}
		}
	default:
		res = Result{Open: true, Reason: "regular trading day"}
	}
	if res.Open && !c.yearComplete(t.Year()) {
		res.Warning = fmt.Sprintf("the holiday list for %d is not marked complete in deploy/nse-holidays.yaml; starting the VM anyway", t.Year())
	}
	return res, nil
}

func (c *Calendar) yearComplete(year int) bool {
	for _, y := range c.YearsComplete {
		if y == year {
			return true
		}
	}
	return false
}
