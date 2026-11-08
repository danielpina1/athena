// Package cron parses five-field cron expressions and finds when they fire.
//
// Fields are minute (0-59), hour (0-23), day of month (1-31), month (1-12) and
// weekday (0-7, where 0 and 7 are Sunday). Each field is a list of items, and an
// item is "*", a value, a range "a-b", optionally followed by "/step" (only on
// "*" and ranges). Names and macros such as "@daily" are not supported.
//
// As in standard cron, when both day and weekday are restricted a day matches if
// either does.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// searchYears bounds how far Next looks for a firing.
const searchYears = 5

// Schedule is a parsed cron expression. It is comparable and safe to copy.
type Schedule struct {
	minute uint64 // bit n set: minute n
	hour   uint32
	dom    uint32 // bit n set: day n (bit 0 unused)
	month  uint16 // bit n set: month n (bit 0 unused)
	dow    uint8  // bit n set: weekday n, Sunday = 0

	// domStar and dowStar record that the field began with "*", which makes the
	// other one the only day restriction instead of an alternative.
	domStar, dowStar bool
}

type fieldSpec struct {
	name     string
	min, max int
}

var (
	minuteSpec = fieldSpec{"minute", 0, 59}
	hourSpec   = fieldSpec{"hour", 0, 23}
	domSpec    = fieldSpec{"day", 1, 31}
	monthSpec  = fieldSpec{"month", 1, 12}
	dowSpec    = fieldSpec{"weekday", 0, 7}
)

// Parse parses a five-field cron expression. An expression that can never fire,
// such as "0 0 30 2 *", is an error.
func Parse(expr string) (Schedule, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("cron: want 5 fields (minute hour day month weekday), got %d", len(f))
	}
	var s Schedule
	var err error
	var m uint64
	if m, err = parseField(f[0], minuteSpec); err != nil {
		return Schedule{}, err
	}
	s.minute = m
	if m, err = parseField(f[1], hourSpec); err != nil {
		return Schedule{}, err
	}
	s.hour = uint32(m)
	if m, err = parseField(f[2], domSpec); err != nil {
		return Schedule{}, err
	}
	s.dom = uint32(m)
	if m, err = parseField(f[3], monthSpec); err != nil {
		return Schedule{}, err
	}
	s.month = uint16(m)
	if m, err = parseField(f[4], dowSpec); err != nil {
		return Schedule{}, err
	}
	// 7 is Sunday too.
	if m&(1<<7) != 0 {
		m = m&^(1<<7) | 1
	}
	s.dow = uint8(m)
	s.domStar = strings.HasPrefix(f[2], "*")
	s.dowStar = strings.HasPrefix(f[4], "*")

	// Only the day of month restricts: some chosen month must have such a day.
	if !s.domStar && s.dowStar && !s.hasDayInMonths() {
		return Schedule{}, fmt.Errorf("cron: day: no chosen month has day(s) %q, so it never fires", f[2])
	}
	return s, nil
}

// hasDayInMonths reports whether any selected month is long enough for any
// selected day of month, counting 29 February.
func (s Schedule) hasDayInMonths() bool {
	maxDays := [13]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	for m := 1; m <= 12; m++ {
		if s.month&(1<<m) == 0 {
			continue
		}
		// Bits 1..maxDays[m] of dom.
		if s.dom&(uint32(1)<<(maxDays[m]+1)-2) != 0 {
			return true
		}
	}
	return false
}

func parseField(field string, spec fieldSpec) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(field, ",") {
		bs, err := parseItem(item, spec)
		if err != nil {
			return 0, fmt.Errorf("cron: %s: %w", spec.name, err)
		}
		set |= bs
	}
	return set, nil
}

func parseItem(item string, spec fieldSpec) (uint64, error) {
	if item == "" {
		return 0, fmt.Errorf("empty list item")
	}
	rng, stepStr, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := parseNum(stepStr)
		if err != nil {
			return 0, fmt.Errorf("bad step %q", stepStr)
		}
		if n < 1 {
			return 0, fmt.Errorf("step must be at least 1, got %d", n)
		}
		step = n
	}
	var lo, hi int
	switch {
	case rng == "*":
		lo, hi = spec.min, spec.max
		if spec.max == 7 {
			hi = 6 // "*" in weekday is 0-6; 7 would only duplicate Sunday
		}
	case strings.Contains(rng, "-"):
		a, b, _ := strings.Cut(rng, "-")
		var err error
		if lo, err = parseNum(a); err != nil {
			return 0, fmt.Errorf("bad range %q", rng)
		}
		if hi, err = parseNum(b); err != nil {
			return 0, fmt.Errorf("bad range %q", rng)
		}
		if lo > hi {
			return 0, fmt.Errorf("range %q runs backwards", rng)
		}
	default:
		if hasStep {
			return 0, fmt.Errorf("step %q needs * or a range", item)
		}
		n, err := parseNum(rng)
		if err != nil {
			return 0, fmt.Errorf("bad value %q", rng)
		}
		lo, hi = n, n
	}
	if lo < spec.min || hi > spec.max {
		return 0, fmt.Errorf("%q is outside %d-%d", item, spec.min, spec.max)
	}
	var set uint64
	for v := lo; v <= hi; v += step {
		set |= 1 << uint(v)
	}
	return set, nil
}

// parseNum accepts plain decimal digits only (no sign, no spaces).
func parseNum(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
	}
	return strconv.Atoi(s)
}

func (s Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	if !s.domStar && !s.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Next returns the first firing strictly after the given time, read on the wall
// clock of after's Location. It returns the zero Time if there is none within
// five years.
//
// On the day clocks go back, a wall-clock time in the repeated hour fires twice.
// On the day they go forward, a time inside the skipped hour does not fire.
func (s Schedule) Next(after time.Time) time.Time {
	loc := after.Location()
	limit := after.AddDate(searchYears, 0, 0)
	t := after.Truncate(time.Minute).Add(time.Minute)

	for t.Before(limit) {
		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			t = advance(t, time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc))
		case !s.dayMatches(t):
			t = advance(t, time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc))
		case s.hour&(1<<uint(t.Hour())) == 0:
			// Absolute, not time.Date: on a repeated hour Date may pick either instant.
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute)
		case s.minute&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// advance returns to if it is later than from, otherwise a step of an hour,
// which guards against wall-clock jumps that land backwards or on the same
// instant.
func advance(from, to time.Time) time.Time {
	if to.After(from) {
		return to
	}
	return from.Add(time.Hour)
}

// TooFrequent reports whether two consecutive firings can be closer than min.
// It looks only at the minutes of the day picked by the hour and minute fields,
// independent of date, and treats the last of the day and the first of the next
// as consecutive.
func (s Schedule) TooFrequent(min time.Duration) bool {
	const day = 24 * 60
	first, prev := -1, -1
	gap := day // a single time of day repeats after a full day
	for h := 0; h < 24; h++ {
		if s.hour&(1<<uint(h)) == 0 {
			continue
		}
		for m := 0; m < 60; m++ {
			if s.minute&(1<<uint(m)) == 0 {
				continue
			}
			at := h*60 + m
			if first < 0 {
				first = at
			} else if at-prev < gap {
				gap = at - prev
			}
			prev = at
		}
	}
	if first >= 0 && first+day-prev < gap {
		gap = first + day - prev
	}
	return time.Duration(gap)*time.Minute < min
}
