package app

import (
	"errors"
	"testing"
	"time"
)

func TestParseCronScheduleValid(t *testing.T) {
	valid := []string{
		"0 9 * * *", "*/15 * * * *", "0 0 1 1 *", "30 4 * * SUN",
		"0 22 * * 1-5", "0 0 * JAN-MAR *", "@hourly", "@daily",
		"@every_5m", "@every_30m", "5 0 * 8 *", "0 */6 * * *",
		"0 9 * * 7", "0 0,12 * * *", "10-20/5 * * * *", "0 9-17/2 * * MON-FRI",
	}
	for _, expr := range valid {
		if _, err := parseCronSchedule(expr); err != nil {
			t.Errorf("parseCronSchedule(%q) unexpected error: %v", expr, err)
		}
	}
}

func TestParseCronScheduleInvalid(t *testing.T) {
	invalid := []string{
		"", "   ", "* * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 32 * *", "* * * 13 *", "* * * 0 *", "* * * * 8",
		"abc * * * *", "*/0 * * * *", "5-1 * * * *", "@bogus",
		"1-2-3 * * * *", "-1 * * * *",
		// Quartz-only extensions are not supported; this parser is Vixie cron.
		// They are rejected rather than misparsed, because a caller migrating a
		// Quartz expression would otherwise get a schedule that fires at the
		// wrong times with no diagnostic.
		"0 9 ? * 1-5", "0 9 * * ?", "0 9 L * *", "0 9 * * 5L",
		"0 9 W * *", "0 9 15W * *", "0 9 ? * 6#3",
		// A six-field expression with seconds (another Quartz shape).
		"0 0 9 * * ?",
	}
	for _, expr := range invalid {
		_, err := parseCronSchedule(expr)
		if err == nil {
			t.Errorf("parseCronSchedule(%q) expected error, got nil", expr)
			continue
		}
		var verr *CronValidationError
		if !errors.As(err, &verr) {
			t.Errorf("parseCronSchedule(%q) error should be *CronValidationError, got %T", expr, err)
		}
	}
}

func TestCronNextFire(t *testing.T) {
	base := time.Date(2026, 9, 8, 10, 0, 0, 0, time.Local) // Tuesday

	tests := []struct {
		expr string
		want time.Time
	}{
		{"0 9 * * *", time.Date(2026, 9, 9, 9, 0, 0, 0, time.Local)},
		{"*/30 * * * *", time.Date(2026, 9, 8, 10, 30, 0, 0, time.Local)},
		{"0 9 * * 1", time.Date(2026, 9, 14, 9, 0, 0, 0, time.Local)}, // next Monday
		{"0 0 1 1 *", time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)},
		{"0 9 * * 7", time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)}, // 7 = Sunday
	}
	for _, tc := range tests {
		sched, err := parseCronSchedule(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		got, ok := sched.next(base)
		if !ok {
			t.Fatalf("next(%q) reported no fire time", tc.expr)
		}
		if !got.Equal(tc.want) {
			t.Errorf("next(%q) from %v = %v, want %v", tc.expr, base, got, tc.want)
		}
	}
}

func TestCronNextNeverFires(t *testing.T) {
	sched, err := parseCronSchedule("0 0 30 2 *") // February 30th
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := sched.next(time.Now()); ok {
		t.Error("expected no fire time for Feb 30")
	}
}

func TestCronDomDowOrSemantics(t *testing.T) {
	// Vixie cron: when both day fields are restricted, either match fires.
	sched, err := parseCronSchedule("0 0 13 * 5") // 13th OR Friday
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	friday := time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local)
	if !sched.matchesDay(friday) {
		t.Error("Friday should match under OR semantics")
	}
	thirteenth := time.Date(2026, 9, 13, 0, 0, 0, 0, time.Local) // Sunday the 13th
	if !sched.matchesDay(thirteenth) {
		t.Error("the 13th should match under OR semantics")
	}
	other := time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local) // Wednesday the 9th
	if sched.matchesDay(other) {
		t.Error("Wednesday the 9th should not match")
	}
}

func TestCronLegacyAliasesStillWork(t *testing.T) {
	base := time.Date(2026, 9, 8, 10, 3, 0, 0, time.Local)
	sched, err := parseCronSchedule("@every_5m")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := sched.next(base)
	if !ok || !got.Equal(time.Date(2026, 9, 8, 10, 5, 0, 0, time.Local)) {
		t.Errorf("@every_5m next = %v (%v), want 10:05", got, ok)
	}
}
