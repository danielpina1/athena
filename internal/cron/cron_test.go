package cron

import (
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s unavailable: %v", name, err)
	}
	return loc
}

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestNext(t *testing.T) {
	utc := time.UTC
	tests := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		{"daily is strictly after", "0 3 * * *", time.Date(2026, 11, 8, 3, 0, 0, 0, utc), time.Date(2026, 11, 9, 3, 0, 0, 0, utc)},
		{"daily same day", "0 3 * * *", time.Date(2026, 11, 8, 2, 59, 30, 0, utc), time.Date(2026, 11, 8, 3, 0, 0, 0, utc)},
		{"every 15 minutes", "*/15 * * * *", time.Date(2026, 11, 8, 10, 7, 0, 0, utc), time.Date(2026, 11, 8, 10, 15, 0, 0, utc)},
		{"monday", "0 9 * * 1", time.Date(2026, 11, 8, 12, 0, 0, 0, utc), time.Date(2026, 11, 9, 9, 0, 0, 0, utc)},
		{"leap day", "0 0 29 2 *", time.Date(2026, 11, 8, 0, 0, 0, 0, utc), time.Date(2028, 2, 29, 0, 0, 0, 0, utc)},
		{"range with step", "0 8-18/5 * * *", time.Date(2026, 11, 8, 9, 0, 0, 0, utc), time.Date(2026, 11, 8, 13, 0, 0, 0, utc)},
		{"list", "5,35 * * * *", time.Date(2026, 11, 8, 10, 5, 0, 0, utc), time.Date(2026, 11, 8, 10, 35, 0, 0, utc)},
		{"year rollover", "0 0 1 1 *", time.Date(2026, 11, 8, 0, 0, 0, 0, utc), time.Date(2027, 1, 1, 0, 0, 0, 0, utc)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustParse(t, tc.expr).Next(tc.after)
			if !got.Equal(tc.want) {
				t.Errorf("Next(%v) = %v, want %v", tc.after, got, tc.want)
			}
		})
	}

	t.Run("monday weekday", func(t *testing.T) {
		got := mustParse(t, "0 9 * * 1").Next(time.Date(2026, 11, 8, 0, 0, 0, 0, utc))
		if got.Weekday() != time.Monday {
			t.Errorf("weekday = %v, want Monday", got.Weekday())
		}
	})
	t.Run("keeps location", func(t *testing.T) {
		loc := mustLoad(t, "Europe/Lisbon")
		got := mustParse(t, "0 3 * * *").Next(time.Date(2026, 11, 8, 0, 0, 0, 0, loc))
		if got.Location() != loc {
			t.Errorf("location = %v, want %v", got.Location(), loc)
		}
	})
}

func TestDayFieldsAreOred(t *testing.T) {
	s := mustParse(t, "0 0 13 * 5")
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var got []int
	for i := 0; i < 5; i++ {
		at = s.Next(at)
		if at.Day() != 13 && at.Weekday() != time.Friday {
			t.Fatalf("%v is neither the 13th nor a Friday", at)
		}
		got = append(got, at.Day())
	}
	// Fridays 6, 13, 20, 27 of November 2026, then Friday 4 December.
	want := []int{6, 13, 20, 27, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("days = %v, want %v", got, want)
		}
	}
	// The 13th that is not a Friday: 13 Sep 2026 is a Sunday.
	if n := s.Next(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)); n.Day() != 13 || n.Month() != time.September {
		t.Errorf("Next = %v, want 13 Sep 2026", n)
	}
}

func TestWeekdaySevenIsSunday(t *testing.T) {
	after := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) // a Sunday
	wantSunday := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for _, expr := range []string{"0 9 * * 7", "0 9 * * 0"} {
		got := mustParse(t, expr).Next(after)
		if !got.Equal(wantSunday) || got.Weekday() != time.Sunday {
			t.Errorf("%q: Next = %v, want %v", expr, got, wantSunday)
		}
	}
	if a, b := mustParse(t, "0 9 * * 7"), mustParse(t, "0 9 * * 0"); a != b {
		t.Errorf("7 and 0 parse differently: %+v vs %+v", a, b)
	}
	// A range ending in 7 includes Sunday: Friday 13 Nov is the first match.
	got := mustParse(t, "0 9 * * 5-7").Next(after)
	if want := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("5-7: Next = %v, want %v", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		expr  string
		field string
	}{
		{"", "5 fields"},
		{"* * * *", "5 fields"},
		{"* * * * * *", "5 fields"},
		{"60 * * * *", "minute"},
		{"* 24 * * *", "hour"},
		{"* * 0 * *", "day"},
		{"* * 32 * *", "day"},
		{"* * * 13 *", "month"},
		{"* * * * 8", "weekday"},
		{"a * * * *", "minute"},
		{"@daily", "5 fields"},
		{"1- * * * *", "minute"},
		{"*/0 * * * *", "minute"},
		{"*/-1 * * * *", "minute"},
		{"5-1 * * * *", "minute"},
		{"1,,2 * * * *", "minute"},
		{"5/10 * * * *", "minute"},
		{"* * * jan *", "month"},
		{"0 0 30 2 *", "day"},
		{"0 0 31 4,6,9,11 *", "day"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := Parse(tc.expr)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name %q", err, tc.field)
			}
		})
	}
	// The 30th of February never exists, but a weekday alternative makes it fire.
	if _, err := Parse("0 0 30 2 1"); err != nil {
		t.Errorf("Parse with weekday alternative: %v", err)
	}
}

func TestSpringForwardSkips(t *testing.T) {
	loc := mustLoad(t, "Europe/Lisbon")
	s := mustParse(t, "30 1 * * *")
	got := s.Next(time.Date(2026, 3, 28, 1, 30, 0, 0, loc))
	want := time.Date(2026, 3, 30, 1, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestFallBackRepeatsWallClock(t *testing.T) {
	loc := mustLoad(t, "Europe/Lisbon")
	s := mustParse(t, "30 1 * * *")
	first := s.Next(time.Date(2026, 10, 25, 0, 0, 0, 0, loc))
	second := s.Next(first)
	if first.Hour() != 1 || first.Minute() != 30 || second.Hour() != 1 || second.Minute() != 30 {
		t.Fatalf("wall clocks = %v, %v, want both 01:30", first, second)
	}
	if d := second.Sub(first); d != time.Hour {
		t.Errorf("gap = %v, want 1h", d)
	}
	if third := s.Next(second); third.Day() != 26 {
		t.Errorf("third = %v, want the 26th", third)
	}
}

func TestTooFrequent(t *testing.T) {
	tests := []struct {
		expr string
		want bool
	}{
		{"*/15 * * * *", false},
		{"*/10 * * * *", true},
		{"0,5 3 * * *", true},
		{"0 3 * * *", false},
		{"* * * * *", true},
		{"*/10 * 1 * *", true}, // the 1st may not fall in a reference week
		{"50 23 * * *", false},
		{"55,5 23,0 * * *", true}, // 23:55 and 00:05 are 10m apart across midnight
		{"55 23 * * *", false},
		{"0,10 0 * * *", true},
		{"59 23 * * *", false},
		{"14,29,44,59 * * * *", false},
		{"0,14 * * * *", true},
	}
	for _, tc := range tests {
		if got := mustParse(t, tc.expr).TooFrequent(15 * time.Minute); got != tc.want {
			t.Errorf("TooFrequent(15m) for %q = %v, want %v", tc.expr, got, tc.want)
		}
	}
	// Wrap from the last minute of the day to the first of the next.
	if !mustParse(t, "59 23 * * *").TooFrequent(25 * time.Hour) {
		t.Error("a daily schedule is closer than 25h to itself")
	}
	if !mustParse(t, "0,59 0,23 * * *").TooFrequent(5 * time.Minute) {
		t.Error("23:59 to 00:00 is one minute apart")
	}
}
