// Package cron parses the five-field schedule a task is written with and
// computes its next run. It is the classic subset every crontab agrees on:
// minute, hour, day of month, month, day of week, each with `*`, lists,
// ranges and steps, plus the @hourly, @daily, @weekly and @monthly
// aliases. Day of month and day of week are OR-ed when both are
// restricted, as Vixie cron does, because that is what people who write
// "0 0 1,15 * 1" expect.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
	expr                          string
}

var aliases = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse reads an expression.
func Parse(expr string) (*Schedule, error) {
	orig := expr
	expr = strings.TrimSpace(strings.ToLower(expr))
	if a, ok := aliases[expr]; ok {
		expr = a
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday), got %d", orig, len(fields))
	}
	s := &Schedule{expr: orig}
	var err error
	if s.minute, _, err = parseField(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", orig, err)
	}
	if s.hour, _, err = parseField(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", orig, err)
	}
	if s.dom, s.domStar, err = parseField(fields[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q: day of month: %w", orig, err)
	}
	if s.month, _, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", orig, err)
	}
	if s.dow, s.dowStar, err = parseField(fields[4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("cron %q: day of week: %w", orig, err)
	}
	// 7 is Sunday too.
	if s.dow&(1<<7) != 0 {
		s.dow |= 1
		s.dow &^= 1 << 7
	}
	return s, nil
}

// parseField returns the bit set for one field and whether it was a bare
// `*`, which matters for the day-of-month/day-of-week rule.
func parseField(f string, lo, hi int, names map[string]int) (uint64, bool, error) {
	var set uint64
	star := f == "*"
	for _, part := range strings.Split(f, ",") {
		if part == "" {
			return 0, false, fmt.Errorf("empty element in %q", f)
		}
		rangePart, stepPart, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepPart)
			if err != nil || n <= 0 {
				return 0, false, fmt.Errorf("step %q", stepPart)
			}
			step = n
		}
		from, to := lo, hi
		if rangePart != "*" {
			a, b, isRange := strings.Cut(rangePart, "-")
			var err error
			if from, err = atom(a, names); err != nil {
				return 0, false, err
			}
			if isRange {
				if to, err = atom(b, names); err != nil {
					return 0, false, err
				}
			} else if hasStep {
				to = hi // "5/10" means from 5 to the end, every 10
			} else {
				to = from
			}
		}
		if from < lo || to > hi || from > to {
			return 0, false, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, star, nil
}

func atom(s string, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[s]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return n, nil
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// Next returns the first time strictly after `after` that matches, in
// after's location. Seconds are dropped: a schedule is a minute, and the
// scheduler compares against the minute it woke up in. It scans field by
// field rather than minute by minute so "0 0 29 2 *" is found in the
// next leap year, not after a four-year loop; a schedule that can never
// match (31 in a 30-day month only) returns the zero time after scanning
// five years rather than spinning forever.
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// Matches reports whether t's minute is one the schedule fires on.
func (s *Schedule) Matches(t time.Time) bool {
	return s.month&(1<<uint(t.Month())) != 0 && s.dayMatches(t) && s.hour&(1<<uint(t.Hour())) != 0 && s.minute&(1<<uint(t.Minute())) != 0
}
