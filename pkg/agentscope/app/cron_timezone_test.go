package app

import (
	"testing"
	"time"
)

// time.Truncate rounds toward absolute, UTC-aligned boundaries. In a zone whose
// UTC offset is not a whole hour, an "hour boundary" built that way lands on
// local HH:30, so no candidate could satisfy both the hour set and the minute
// set: next() reported ok=false for EVERY expression and schedule creation
// failed with HTTP 400 for every deployment in those zones. Each case below
// uses a non-hour offset on purpose — whole-hour zones (Asia/Shanghai, UTC)
// hide the bug, which is why the original tests did not catch it.
func TestCronNextNonHourOffsetZones(t *testing.T) {
	tests := []struct {
		name       string
		offsetSecs int
		expr       string
		// from is a LOCAL wall clock in the zone under test.
		from     [6]int // y, m, d, hh, mm, ss
		want     [5]int // y, m, d, hh, mm
		wantOK   bool
		noteWant string
	}{
		{"kolkata daily 10:00", 5*3600 + 30*60, "0 10 * * *",
			[6]int{2026, 9, 8, 9, 0, 0}, [5]int{2026, 9, 8, 10, 0}, true, ""},
		{"kolkata every 30 min", 5*3600 + 30*60, "*/30 * * * *",
			[6]int{2026, 9, 8, 9, 7, 0}, [5]int{2026, 9, 8, 9, 30}, true, ""},
		{"kolkata hourly on the half hour", 5*3600 + 30*60, "0 * * * *",
			[6]int{2026, 9, 8, 9, 30, 0}, [5]int{2026, 9, 8, 10, 0}, true, ""},
		{"tehran plus 3:30", 3*3600 + 30*60, "15 6 * * *",
			[6]int{2026, 9, 8, 5, 0, 0}, [5]int{2026, 9, 8, 6, 15}, true, ""},
		{"yangon plus 6:30 rolls to next day", 6*3600 + 30*60, "0 0 * * *",
			[6]int{2026, 9, 8, 10, 0, 0}, [5]int{2026, 9, 9, 0, 0}, true, ""},
		{"darwin plus 9:30", 9*3600 + 30*60, "45 14 * * *",
			[6]int{2026, 9, 8, 8, 0, 0}, [5]int{2026, 9, 8, 14, 45}, true, ""},
		// 2026-09-09 is a Wednesday, so a weekday-only 08:00 schedule at
		// 12:00 must roll to Thursday 2026-09-10.
		{"st_johns minus 3:30 weekdays", -(3*3600 + 30*60), "0 8 * * 1-5",
			[6]int{2026, 9, 9, 12, 0, 0}, [5]int{2026, 9, 10, 8, 0}, true, ""},
		{"kathmandu plus 5:45", 5*3600 + 45*60, "30 7 * * *",
			[6]int{2026, 9, 8, 6, 0, 0}, [5]int{2026, 9, 8, 7, 30}, true, ""},
		{"impossible date in any zone", 5*3600 + 30*60, "0 0 30 2 *",
			[6]int{2026, 9, 8, 6, 0, 0}, [5]int{}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc := time.FixedZone("test", tc.offsetSecs)
			from := time.Date(tc.from[0], time.Month(tc.from[1]), tc.from[2],
				tc.from[3], tc.from[4], tc.from[5], 0, loc)

			c, err := parseCronSchedule(tc.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.expr, err)
			}

			// Guard the scan itself: a non-advancing step would hang here
			// instead of failing, which is how the DST fall-back loop was
			// caught.
			type result struct {
				t  time.Time
				ok bool
			}
			done := make(chan result, 1)
			go func() {
				nt, nok := c.next(from)
				done <- result{nt, nok}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("next(%v) for %q did not terminate", from, tc.expr)
			}

			if got.ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (next = %v)", got.ok, tc.wantOK, got.t)
			}
			if !tc.wantOK {
				return
			}
			want := time.Date(tc.want[0], time.Month(tc.want[1]), tc.want[2],
				tc.want[3], tc.want[4], 0, 0, loc)
			if !got.t.Equal(want) {
				t.Errorf("next = %v, want %v", got.t, want)
			}
			if !got.t.After(from) {
				t.Errorf("next %v must be strictly after %v", got.t, from)
			}
			if got.t.Second() != 0 || got.t.Nanosecond() != 0 {
				t.Errorf("next %v must be aligned to the minute grid", got.t)
			}
		})
	}
}

// Sub-minute precision must be dropped on the LOCAL calendar, not the absolute
// one, and the result must still be strictly later.
func TestCronNextNonHourOffsetSubMinute(t *testing.T) {
	loc := time.FixedZone("half-hour", 5*3600+30*60)
	from := time.Date(2026, 9, 8, 10, 0, 30, 500, loc)

	c, err := parseCronSchedule("* * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	next, ok := c.next(from)
	if !ok {
		t.Fatal("an every-minute expression must always have a next fire time")
	}
	want := time.Date(2026, 9, 8, 10, 1, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

// A leap-day expression is syntactically valid and fires once every four years.
// The previous one-year scan window rejected it at the API boundary as
// "expression never fires", which is wrong: it does fire, just not this year.
func TestCronNextLeapDayIsAccepted(t *testing.T) {
	c, err := parseCronSchedule("0 0 29 2 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	next, ok := c.next(from)
	if !ok {
		t.Fatalf("a leap-day expression must fire within the %d-year scan window", cronScanYears)
	}
	want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

// DST transitions are where a calendar-field walk can stop advancing: on a
// fall-back day an ambiguous local hour occurs twice and time.Date always
// resolves it to the FIRST occurrence, so "rebuild hour+1" lands back on the
// same instant. next() must step past it rather than spin.
func TestCronNextAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	nextWithTimeout := func(t *testing.T, expr string, from time.Time) (time.Time, bool) {
		t.Helper()
		c, perr := parseCronSchedule(expr)
		if perr != nil {
			t.Fatalf("parse %q: %v", expr, perr)
		}
		type result struct {
			t  time.Time
			ok bool
		}
		done := make(chan result, 1)
		go func() {
			nt, nok := c.next(from)
			done <- result{nt, nok}
		}()
		select {
		case r := <-done:
			return r.t, r.ok
		case <-time.After(5 * time.Second):
			t.Fatalf("next(%q) from %v did not terminate (DST spin)", expr, from)
			return time.Time{}, false
		}
	}

	t.Run("spring forward gap is skipped", func(t *testing.T) {
		// 2026-03-08: 02:00 EST jumps to 03:00 EDT, so 02:30 does not exist.
		from := time.Date(2026, 3, 8, 1, 0, 0, 0, ny)
		next, ok := nextWithTimeout(t, "30 2 * * *", from)
		if !ok {
			t.Fatal("expected a fire time")
		}
		if next.Day() == 8 && next.Month() == time.March {
			t.Errorf("fired on the gap day: %v", next)
		}
		if next.Hour() != 2 || next.Minute() != 30 {
			t.Errorf("next = %v, want local 02:30", next)
		}
	})

	t.Run("fall back repeated hour does not hang", func(t *testing.T) {
		// 2026-11-01: 02:00 EDT falls back to 01:00 EST, so local 01:xx
		// happens twice. A schedule at 05:30 later the same day must be
		// reachable without spinning inside the repeated hour.
		from := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
		next, ok := nextWithTimeout(t, "30 5 * * *", from)
		if !ok {
			t.Fatal("expected a fire time")
		}
		want := time.Date(2026, 11, 1, 5, 30, 0, 0, ny)
		if !next.Equal(want) {
			t.Errorf("next = %v, want %v", next, want)
		}
	})

	t.Run("schedule inside the repeated hour advances", func(t *testing.T) {
		// The worst case for the rebuild-from-fields approach: the target
		// hour itself is the ambiguous one.
		from := time.Date(2026, 11, 1, 0, 30, 0, 0, ny)
		next, ok := nextWithTimeout(t, "30 1 * * *", from)
		if !ok {
			t.Fatal("expected a fire time")
		}
		if !next.After(from) {
			t.Errorf("next %v must be after %v", next, from)
		}
		if next.Hour() != 1 || next.Minute() != 30 {
			t.Errorf("next = %v, want local 01:30", next)
		}
		if next.Day() != 1 || next.Month() != time.November {
			t.Errorf("next = %v, want it on the transition day itself", next)
		}
	})
}

// Every expression must either produce a strictly-later fire time or report
// that it never fires — never spin, and never return a time in the past.
func TestCronNextAlwaysAdvances(t *testing.T) {
	loc := time.FixedZone("odd", 5*3600+45*60)
	from := time.Date(2026, 9, 8, 12, 34, 56, 0, loc)
	for _, expr := range []string{
		"* * * * *", "0 0 1 1 *", "*/7 */3 * * *", "59 23 31 12 *",
		"0 0 29 2 *", "30 2 * * *", "0 */12 * * *", "@every_5m", "@every_12h",
	} {
		c, err := parseCronSchedule(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		type result struct {
			t  time.Time
			ok bool
		}
		done := make(chan result, 1)
		go func() {
			nt, nok := c.next(from)
			done <- result{nt, nok}
		}()
		var got result
		select {
		case got = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("next(%q) did not terminate", expr)
		}
		if !got.ok {
			continue
		}
		if !got.t.After(from) {
			t.Errorf("%q: next %v is not after %v", expr, got.t, from)
		}
		if got.t.Second() != 0 || got.t.Nanosecond() != 0 {
			t.Errorf("%q: next %v is not on the minute grid", expr, got.t)
		}
	}
}

// On a DST fall-back day, dropping sub-minute precision and rebuilding from
// local fields can move BACKWARD: time.Date resolves an ambiguous local time to
// its FIRST occurrence, so rebuilding 01:30 at 01:30 EST yields 01:30 EDT, an
// hour earlier in absolute terms. A single compensating minute left the result
// still before `after`, which would let Create hand the scheduler a RunAt in the
// past.
func TestCronNextIsStrictlyAfterOnFallBackDay(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	c, err := parseCronSchedule("31 1 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 01:30 EST is the FIRST occurrence of that wall clock; 01:30 EDT would be
	// an hour earlier in absolute terms.
	from := time.Date(2026, 11, 1, 1, 30, 0, 0, ny)
	type result struct {
		t  time.Time
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		nt, nok := c.next(from)
		done <- result{nt, nok}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("next() did not terminate")
	}

	if !got.ok {
		t.Fatal("expected a fire time")
	}
	if !got.t.After(from) {
		t.Errorf("next = %v (%s) is not strictly after %v (%s); a RunAt in the past "+
			"would fire immediately", got.t, got.t.Format(time.RFC3339), from, from.Format(time.RFC3339))
	}
	if got.t.Minute() != 31 || got.t.Hour() != 1 {
		t.Errorf("next = %v, want local 01:31", got.t)
	}
	if got.t.Second() != 0 || got.t.Nanosecond() != 0 {
		t.Errorf("next = %v is not on the minute grid", got.t)
	}
}

// Sub-minute precision on a fall-back day must also land strictly later.
func TestCronNextSubMinuteOnFallBackDay(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	c, err := parseCronSchedule("* * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 11, 1, 1, 30, 45, 0, ny)
	next, ok := c.next(from)
	if !ok {
		t.Fatal("an every-minute expression must always have a next fire time")
	}
	if !next.After(from) {
		t.Errorf("next = %v is not after %v", next, from)
	}
}
