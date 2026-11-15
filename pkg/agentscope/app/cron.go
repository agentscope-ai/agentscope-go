package app

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron expression parsing, validation, and next-fire computation.
//
// Upstream #2442 validates a cron expression BEFORE persisting a schedule;
// the previous Go behavior silently folded every unrecognized expression
// (including standard five-field cron like "0 9 * * *") into an hourly
// interval and recorded it as active. This file replaces that: expressions
// are fully parsed and validated, and a schedule fires exactly when its
// expression says — or creation fails with a *CronValidationError.

// CronValidationError reports a rejected cron expression. Routes map it to
// HTTP 400 (the client supplied a bad schedule), distinct from internal
// scheduling failures.
type CronValidationError struct {
	Expr   string
	Reason string
}

func (e *CronValidationError) Error() string {
	return fmt.Sprintf("invalid cron expression %q: %s", e.Expr, e.Reason)
}

// cronAliases maps @-prefixed shorthands to five-field expressions.
//
// This changes behavior in two independent ways. The previous parser was
// parseCronInterval, which recognized only @hourly, @daily, @every_5m,
// @every_10m and @every_30m. Everything else, including @every_1h, @every_12h,
// @weekly, @yearly and any five-field expression such as "0 9 * * *", fell
// through to `default: return time.Hour`. It also set schedule.Interval and left
// RunAt zero, and InMemoryScheduler runs an Interval task immediately before
// starting the ticker. So:
//
//	expression     old actual behavior            new behavior
//	@every_5m      fire at once, then 10:08, ...  10:05, 10:10, ...
//	@every_12h     fire at once, then hourly      00:00 and 12:00
//	0 9 * * *      fire at once, then hourly      daily at 09:00
//
// The old-behavior column is the one operators notice: nothing fires at creation
// any more. Schedules persisted before this change keep their stored expression
// but fire on the new grid.
var cronAliases = map[string]string{
	"@hourly":    "0 * * * *",
	"@daily":     "0 0 * * *",
	"@midnight":  "0 0 * * *",
	"@weekly":    "0 0 * * 0",
	"@monthly":   "0 0 1 * *",
	"@yearly":    "0 0 1 1 *",
	"@annually":  "0 0 1 1 *",
	"@every_5m":  "*/5 * * * *",
	"@every_10m": "*/10 * * * *",
	"@every_30m": "*/30 * * * *",
	"@every_1h":  "0 * * * *",
	"@every_12h": "0 */12 * * *",
}

// cronSchedule is a parsed five-field cron expression. Fields are bitmask
// sets over their legal ranges. When both day-of-month and day-of-week are
// restricted, a day matches if EITHER matches (Vixie cron semantics).
type cronSchedule struct {
	minutes     []bool   // index 0-59
	hours       []bool   // index 0-23
	daysOfMonth [32]bool // index 1-31
	months      [13]bool // index 1-12
	daysOfWeek  [7]bool  // 0-6, Sunday=0
	domWildcard bool
	dowWildcard bool
}

var cronMonthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var cronDowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// parseCronSchedule parses and validates a cron expression. It accepts the
// standard five fields (minute hour day-of-month month day-of-week) with
// "*", values, ranges (a-b), steps (*/n, a-b/n, a/n), and comma lists, plus
// month/day-of-week names and @-aliases. Day-of-week accepts 0-7 where both
// 0 and 7 are Sunday.
func parseCronSchedule(expr string) (*cronSchedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, &CronValidationError{Expr: expr, Reason: "expression is empty"}
	}
	if strings.HasPrefix(trimmed, "@") {
		expanded, ok := cronAliases[strings.ToLower(trimmed)]
		if !ok {
			return nil, &CronValidationError{Expr: expr, Reason: "unknown alias"}
		}
		trimmed = expanded
	}

	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return nil, &CronValidationError{
			Expr:   expr,
			Reason: fmt.Sprintf("expected 5 fields, got %d", len(fields)),
		}
	}

	s := &cronSchedule{}
	var err error
	if s.minutes, err = parseCronField(fields[0], 0, 59, nil); err != nil {
		return nil, &CronValidationError{Expr: expr, Reason: "minute: " + err.Error()}
	}
	if s.hours, err = parseCronField(fields[1], 0, 23, nil); err != nil {
		return nil, &CronValidationError{Expr: expr, Reason: "hour: " + err.Error()}
	}
	var dom []bool
	if dom, err = parseCronField(fields[2], 1, 31, nil); err != nil {
		return nil, &CronValidationError{Expr: expr, Reason: "day-of-month: " + err.Error()}
	}
	copy(s.daysOfMonth[:], dom)
	var months []bool
	if months, err = parseCronField(fields[3], 1, 12, cronMonthNames); err != nil {
		return nil, &CronValidationError{Expr: expr, Reason: "month: " + err.Error()}
	}
	copy(s.months[:], months)
	// day-of-week: 0-7 with 7 folded to Sunday.
	dow, err := parseCronField(fields[4], 0, 7, cronDowNames)
	if err != nil {
		return nil, &CronValidationError{Expr: expr, Reason: "day-of-week: " + err.Error()}
	}
	for v := 0; v <= 6; v++ {
		s.daysOfWeek[v] = dow[v]
	}
	if len(dow) > 7 && dow[7] {
		s.daysOfWeek[0] = true
	}

	s.domWildcard = fields[2] == "*"
	s.dowWildcard = fields[4] == "*"
	return s, nil
}

// parseCronField parses one cron field into a bitmask slice covering
// [min, max]. names provides optional lowercase name aliases (month/dow).
func parseCronField(field string, min, max int, names map[string]int) ([]bool, error) {
	set := make([]bool, max+2) // +2 so dow index 7 fits
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty list element")
		}
		rangeSpec := part
		step := 1
		// SplitN with n=2 keeps any extra slashes inside the step token, so
		// "a/b/c" fails the Atoi below with "invalid step" rather than being
		// silently accepted.
		if slash := strings.SplitN(part, "/", 2); len(slash) == 2 {
			rangeSpec = slash[0]
			n, err := strconv.Atoi(slash[1])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid step %q", slash[1])
			}
			step = n
		}

		var lo, hi int
		switch {
		case rangeSpec == "*":
			lo, hi = min, max
		case strings.Contains(rangeSpec, "-"):
			bounds := strings.SplitN(rangeSpec, "-", 2)
			a, err := parseCronValue(bounds[0], min, max, names)
			if err != nil {
				return nil, err
			}
			b, err := parseCronValue(bounds[1], min, max, names)
			if err != nil {
				return nil, err
			}
			if a > b {
				return nil, fmt.Errorf("inverted range %q", rangeSpec)
			}
			lo, hi = a, b
		default:
			v, err := parseCronValue(rangeSpec, min, max, names)
			if err != nil {
				return nil, err
			}
			if step == 1 {
				lo, hi = v, v
			} else {
				// "a/n" means every n from a to the field maximum.
				lo, hi = v, max
			}
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	return set, nil
}

func parseCronValue(tok string, min, max int, names map[string]int) (int, error) {
	lower := strings.ToLower(strings.TrimSpace(tok))
	if names != nil {
		if v, ok := names[lower]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(lower)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", tok)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, min, max)
	}
	return n, nil
}

// matchesDay reports whether the calendar day of t matches the schedule,
// applying Vixie cron OR semantics when both day fields are restricted.
//
// "Restricted" means the field was not the literal "*" (see parseCronSchedule):
// "*/1" and "?" therefore count as restricted, exactly as in Vixie cron. That
// makes "0 9 */1 * 1-5" fire every day, not only on weekdays — surprising but
// standard, so it is called out here rather than "fixed".
func (c *cronSchedule) matchesDay(t time.Time) bool {
	if !c.months[int(t.Month())] {
		return false
	}
	domMatch := c.daysOfMonth[t.Day()]
	dowMatch := c.daysOfWeek[int(t.Weekday())]
	switch {
	case c.domWildcard && c.dowWildcard:
		return true
	case c.domWildcard:
		return dowMatch
	case c.dowWildcard:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

// cronScanYears bounds the search for the next fire time.
//
// Eight years, not one: a leap-day expression such as "0 0 29 2 *" is
// syntactically valid and the old one-year window rejected it at the API
// boundary as "never fires". Four years would cover the normal cadence, and
// eight also covers the gap around a century year that is not a leap year —
// from 2097 the next 29 February is 2104, because 2100 is divisible by 100 but
// not by 400. Cost is irrelevant: non-matching days are skipped wholesale, so
// even a never-firing expression is a few thousand iterations.
//
// This is still a window, not a proof of impossibility. An expression whose
// next fire is beyond it is reported as never firing.
const cronScanYears = 8

// next returns the first fire time after `after`, aligned to the minute grid of
// after's own location, or ok=false when the expression never fires within
// cronScanYears (e.g. "0 0 30 2 *"). Non-matching days are skipped wholesale.
//
// The result is strictly after `after` in absolute time. On a DST fall-back day
// that is not the same as a later wall-clock reading — the repeated local hour
// is walked through — but the guarantee callers actually need (never hand the
// scheduler a RunAt in the past) holds.
//
// Every step is rebuilt with time.Date from local calendar fields on purpose.
// time.Truncate rounds toward absolute, UTC-aligned boundaries: in a zone whose
// UTC offset is not a whole hour (Asia/Kolkata +5:30, Asia/Tehran +3:30,
// Asia/Yangon +6:30, Australia/Darwin +9:30, America/St_Johns -3:30, ...) an
// "hour boundary" produced by Truncate lands on local HH:30, so no candidate
// can ever satisfy both the hour and the minute set and next() reports
// ok=false for EVERY expression. That made schedule creation fail with HTTP
// 400 for all deployments in those zones.
func (c *cronSchedule) next(after time.Time) (time.Time, bool) {
	loc := after.Location()
	// Drop sub-minute precision on the local calendar, then step to the next
	// whole minute so the result is strictly after `after`.
	t := time.Date(after.Year(), after.Month(), after.Day(),
		after.Hour(), after.Minute(), 0, 0, loc)
	// A loop, not a single step: dropping sub-minute precision and rebuilding
	// from local fields can move BACKWARD on a DST fall-back day, because
	// time.Date resolves an ambiguous local time to its first occurrence (at
	// 01:30 EST, rebuilding 01:30 yields 01:30 EDT, an hour earlier in
	// absolute terms). One +1 minute would then still be before `after`, and
	// Create could hand the scheduler a RunAt in the past. Stepping until
	// strictly after is bounded by the offset span (at most an hour of
	// minutes) and stays on the minute grid.
	for !t.After(after) {
		t = t.Add(time.Minute)
	}
	limit := after.AddDate(cronScanYears, 0, 0)
	for t.Before(limit) {
		prev := t
		// fallback is an absolute step guaranteed to move forward, used when
		// the calendar rebuild below lands back on prev.
		var fallback time.Duration
		switch {
		case !c.matchesDay(t):
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			fallback = 24 * time.Hour
		case !c.hours[t.Hour()]:
			// Top of the next local hour.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			fallback = time.Hour
		case !c.minutes[t.Minute()]:
			t = t.Add(time.Minute)
			fallback = time.Minute
		default:
			return t, true
		}
		// Termination guard. On a DST fall-back day an ambiguous local hour
		// occurs twice, and time.Date ALWAYS resolves it to the first
		// occurrence: at 01:00 EST, Date(...,1,0,0,0)+1h is 01:00 EST again.
		// Without this the hour branch would spin forever. Stepping by an
		// absolute duration is monotonic by construction, so the walk either
		// finds a fire time or runs out of the scan window.
		if !t.After(prev) {
			t = prev.Add(fallback)
		}
	}
	return time.Time{}, false
}
