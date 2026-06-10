// Copyright (C) 2012 Rob Figueiredo.
// Originally derived from github.com/robfig/cron/v3 (MIT License).
//
// Credo adapts only cron expression parsing and next-fire calculation,
// trimmed to standard 5-field Vixie cron plus descriptors.

package worker

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Schedule represents a compiled cron schedule.
type Schedule struct {
	expr string
	next nextCalculator
}

// Next returns the next fire time after now.
func (s *Schedule) Next(now time.Time) time.Time {
	if s == nil || s.next == nil {
		return time.Time{}
	}
	return s.next.Next(now)
}

// String returns the original cron expression.
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.expr
}

// ParseSchedule parses a cron expression into a compiled schedule.
//
// The supported syntax is standard 5-field Vixie cron — minute, hour,
// day-of-month, month, day-of-week — with lists ("1,15"), ranges ("1-5"),
// steps ("*/10", "8-18/2"), month and weekday names ("jan", "sat"), "?" as
// an alias for "*", and 7 accepted as Sunday. The descriptors @hourly,
// @daily (alias @midnight), @weekly, @monthly, and "@every <duration>" are
// also accepted. Schedules are evaluated in the server's local time zone
// and fire at second 0 of the matching minute; for sub-minute periods use
// "@every <duration>".
//
// As in crontab(5), when both the day-of-month and day-of-week fields are
// restricted (neither is "*"), the schedule fires when EITHER matches:
// "0 0 13 * fri" runs on the 13th of the month AND on every Friday. Note
// that a step applied to "*" (e.g. "*/2" in day-of-month) counts as
// restricted for this rule.
func ParseSchedule(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("worker: schedule expression must not be empty")
	}

	compiled, err := parseCron(expr)
	if err != nil {
		return nil, fmt.Errorf("worker: parse schedule %q: %w", expr, err)
	}

	return &Schedule{expr: expr, next: compiled}, nil
}

type nextCalculator interface {
	Next(time.Time) time.Time
}

func parseCron(spec string) (nextCalculator, error) {
	if strings.HasPrefix(spec, "@") {
		return parseDescriptor(spec)
	}
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		return nil, fmt.Errorf("timezone prefixes are not supported; schedules run in the server's local time")
	}

	fields := strings.Fields(spec)
	if len(fields) == 6 {
		return nil, fmt.Errorf("the 6-field form with seconds is not supported; use a 5-field expression (sub-minute periods: @every)")
	}
	if len(fields) != 5 {
		return nil, fmt.Errorf("expected exactly 5 fields, found %d: %v", len(fields), fields)
	}

	fieldBounds := []bounds{minutes, hours, dom, months, dow}
	var parsed [5]uint64
	var err error
	for i, field := range fields {
		parsed[i], err = getField(field, fieldBounds[i])
		if err != nil {
			return nil, err
		}
	}

	return &specSchedule{
		Minute: parsed[0],
		Hour:   parsed[1],
		Dom:    parsed[2],
		Month:  parsed[3],
		Dow:    parsed[4],
	}, nil
}

// specSchedule is a compiled 5-field cron expression. Each field is a bit
// set of allowed values; starBit marks fields written as "*" (or "?"),
// which matters for the day-of-month/day-of-week OR rule.
type specSchedule struct {
	Minute uint64
	Hour   uint64
	Dom    uint64
	Month  uint64
	Dow    uint64
}

type bounds struct {
	min, max uint
	names    map[string]uint
	sunday7  bool
}

var (
	minutes = bounds{min: 0, max: 59}
	hours   = bounds{min: 0, max: 23}
	dom     = bounds{min: 1, max: 31}
	months  = bounds{min: 1, max: 12, names: map[string]uint{
		"jan": 1,
		"feb": 2,
		"mar": 3,
		"apr": 4,
		"may": 5,
		"jun": 6,
		"jul": 7,
		"aug": 8,
		"sep": 9,
		"oct": 10,
		"nov": 11,
		"dec": 12,
	}}
	dow = bounds{min: 0, max: 6, sunday7: true, names: map[string]uint{
		"sun": 0,
		"mon": 1,
		"tue": 2,
		"wed": 3,
		"thu": 4,
		"fri": 5,
		"sat": 6,
	}}
)

const starBit = 1 << 63

// Next computes the first activation strictly after t, in t's location.
// Activations are minute-resolution: they fire at second 0.
func (s *specSchedule) Next(t time.Time) time.Time {
	// Round up to the next whole minute.
	t = t.Add(time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond())*time.Nanosecond)
	added := false
	yearLimit := t.Year() + 5

wrap:
	if t.Year() > yearLimit {
		return time.Time{}
	}

	for 1<<uint(t.Month())&s.Month == 0 {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		}
		t = t.AddDate(0, 1, 0)
		if t.Month() == time.January {
			goto wrap
		}
	}

	for !dayMatches(s, t) {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		}
		t = t.AddDate(0, 0, 1)
		// In daylight-saving locations, AddDate can land off midnight when
		// the clock shifts; snap back to the start of the day.
		if t.Hour() != 0 {
			if t.Hour() > 12 {
				t = t.Add(time.Duration(24-t.Hour()) * time.Hour)
			} else {
				t = t.Add(-time.Duration(t.Hour()) * time.Hour)
			}
		}
		if t.Day() == 1 {
			goto wrap
		}
	}

	for 1<<uint(t.Hour())&s.Hour == 0 {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
		}
		t = t.Add(time.Hour)
		if t.Hour() == 0 {
			goto wrap
		}
	}

	for 1<<uint(t.Minute())&s.Minute == 0 {
		if !added {
			added = true
			t = t.Truncate(time.Minute)
		}
		t = t.Add(time.Minute)
		if t.Minute() == 0 {
			goto wrap
		}
	}

	return t
}

// dayMatches implements the crontab(5) day rule: when both Dom and Dow are
// restricted (no starBit), the day matches if EITHER field matches;
// otherwise both must match (a "*" field matches every day anyway).
func dayMatches(s *specSchedule, t time.Time) bool {
	domMatch := 1<<uint(t.Day())&s.Dom > 0
	dowMatch := 1<<uint(t.Weekday())&s.Dow > 0
	if s.Dom&starBit > 0 || s.Dow&starBit > 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

type constantDelaySchedule struct {
	Delay time.Duration
}

func every(duration time.Duration) constantDelaySchedule {
	if duration < time.Second {
		duration = time.Second
	}
	return constantDelaySchedule{Delay: duration - time.Duration(duration.Nanoseconds())%time.Second}
}

func (s constantDelaySchedule) Next(t time.Time) time.Time {
	return t.Add(s.Delay - time.Duration(t.Nanosecond())*time.Nanosecond)
}

func getField(field string, r bounds) (uint64, error) {
	var bits uint64
	ranges := strings.FieldsFunc(field, func(r rune) bool { return r == ',' })
	for _, expr := range ranges {
		bit, err := getRange(expr, r)
		if err != nil {
			return bits, err
		}
		bits |= bit
	}
	return bits, nil
}

func getRange(expr string, r bounds) (uint64, error) {
	var (
		start, end, step uint
		rangeAndStep     = strings.Split(expr, "/")
		lowAndHigh       = strings.Split(rangeAndStep[0], "-")
		singleDigit      = len(lowAndHigh) == 1
		err              error
	)

	var extra uint64
	if lowAndHigh[0] == "*" || lowAndHigh[0] == "?" {
		start = r.min
		end = r.max
		extra = starBit
	} else {
		start, err = parseIntOrName(lowAndHigh[0], r)
		if err != nil {
			return 0, err
		}
		switch len(lowAndHigh) {
		case 1:
			end = start
		case 2:
			end, err = parseIntOrName(lowAndHigh[1], r)
			if err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("too many hyphens: %s", expr)
		}
	}

	switch len(rangeAndStep) {
	case 1:
		step = 1
	case 2:
		step, err = mustParseUint(rangeAndStep[1])
		if err != nil {
			return 0, err
		}
		if singleDigit {
			end = r.max
		}
		if step > 1 {
			extra = 0
		}
	default:
		return 0, fmt.Errorf("too many slashes: %s", expr)
	}

	if start < r.min {
		return 0, fmt.Errorf("beginning of range (%d) below minimum (%d): %s", start, r.min, expr)
	}
	if end > r.max {
		return 0, fmt.Errorf("end of range (%d) above maximum (%d): %s", end, r.max, expr)
	}
	if start > end {
		return 0, fmt.Errorf("beginning of range (%d) beyond end of range (%d): %s", start, end, expr)
	}
	if step == 0 {
		return 0, fmt.Errorf("step of range should be a positive number: %s", expr)
	}

	return getBits(start, end, step) | extra, nil
}

func parseIntOrName(expr string, r bounds) (uint, error) {
	if r.names != nil {
		if namedInt, ok := r.names[strings.ToLower(expr)]; ok {
			return namedInt, nil
		}
	}
	num, err := mustParseUint(expr)
	if err != nil {
		return 0, err
	}
	if r.sunday7 && num == 7 {
		return 0, nil
	}
	return num, nil
}

func mustParseUint(expr string) (uint, error) {
	num, err := strconv.Atoi(expr)
	if err != nil {
		return 0, fmt.Errorf("failed to parse int from %s: %w", expr, err)
	}
	if num < 0 {
		return 0, fmt.Errorf("negative number (%d) not allowed: %s", num, expr)
	}
	return uint(num), nil
}

func getBits(min, max, step uint) uint64 {
	var bits uint64
	if step == 1 {
		return ^(math.MaxUint64 << (max + 1)) & (math.MaxUint64 << min)
	}
	for i := min; i <= max; i += step {
		bits |= 1 << i
	}
	return bits
}

func all(r bounds) uint64 {
	return getBits(r.min, r.max, 1) | starBit
}

func parseDescriptor(expr string) (nextCalculator, error) {
	switch expr {
	case "@yearly", "@annually":
		return nil, fmt.Errorf(`descriptor %s is not supported; use "0 0 1 1 *"`, expr)
	case "@monthly":
		return &specSchedule{
			Minute: 1 << minutes.min,
			Hour:   1 << hours.min,
			Dom:    1 << dom.min,
			Month:  all(months),
			Dow:    all(dow),
		}, nil
	case "@weekly":
		return &specSchedule{
			Minute: 1 << minutes.min,
			Hour:   1 << hours.min,
			Dom:    all(dom),
			Month:  all(months),
			Dow:    1 << dow.min,
		}, nil
	case "@daily", "@midnight":
		return &specSchedule{
			Minute: 1 << minutes.min,
			Hour:   1 << hours.min,
			Dom:    all(dom),
			Month:  all(months),
			Dow:    all(dow),
		}, nil
	case "@hourly":
		return &specSchedule{
			Minute: 1 << minutes.min,
			Hour:   all(hours),
			Dom:    all(dom),
			Month:  all(months),
			Dow:    all(dow),
		}, nil
	}

	const everyPrefix = "@every "
	if strings.HasPrefix(expr, everyPrefix) {
		duration, err := time.ParseDuration(expr[len(everyPrefix):])
		if err != nil {
			return nil, fmt.Errorf("failed to parse duration %s: %w", expr, err)
		}
		return every(duration), nil
	}

	return nil, fmt.Errorf("unrecognized descriptor: %s", expr)
}
