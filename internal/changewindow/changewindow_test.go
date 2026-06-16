// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package changewindow

import (
	"testing"
	"time"
)

// mustLoc loads a timezone or fails the test. Used to write the
// "now" timestamps below in local time so the test reads naturally.
func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load tz %q: %v", name, err)
	}
	return loc
}

func TestWindow_Validate(t *testing.T) {
	cases := []struct {
		name   string
		w      Window
		errSub string
	}{
		{"missing name", Window{Timezone: "UTC", StartLocal: "16:00", EndLocal: "21:00"}, "name is required"},
		{"missing tz", Window{Name: "x", StartLocal: "16:00", EndLocal: "21:00"}, "timezone is required"},
		{"bad tz", Window{Name: "x", Timezone: "Earth/Atlantis", StartLocal: "16:00", EndLocal: "21:00"}, "unknown timezone"},
		{"bad start", Window{Name: "x", Timezone: "UTC", StartLocal: "25:00", EndLocal: "21:00"}, "start_local"},
		{"bad end", Window{Name: "x", Timezone: "UTC", StartLocal: "16:00", EndLocal: "x"}, "end_local"},
		{"bad dow", Window{Name: "x", Timezone: "UTC", StartLocal: "16:00", EndLocal: "21:00", DaysOfWeek: []int{7}}, "days_of_week"},
		{"bad from", Window{Name: "x", Timezone: "UTC", StartLocal: "16:00", EndLocal: "21:00", EffectiveFrom: "yesterday"}, "effective_from"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.w.Validate()
			if err == nil || !contains(err.Error(), c.errSub) {
				t.Fatalf("expected error containing %q, got %v", c.errSub, err)
			}
		})
	}

	// Happy path
	ok := Window{Name: "peak", Timezone: "America/Chicago", StartLocal: "16:00", EndLocal: "21:00"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid window unexpectedly rejected: %v", err)
	}
}

func TestWindow_IsActive_WeekdayInWindow(t *testing.T) {
	// Summer peak demand window: 16:00-21:00 CT weekdays
	w := Window{
		Name:       "summer peak",
		Timezone:   "America/Chicago",
		StartLocal: "16:00",
		EndLocal:   "21:00",
		DaysOfWeek: []int{1, 2, 3, 4, 5}, // Mon..Fri
	}
	ct := mustLoc(t, "America/Chicago")

	// Tuesday 17:00 CT — clearly inside.
	tue1700 := time.Date(2026, time.July, 14, 17, 0, 0, 0, ct)
	if !w.IsActive(tue1700) {
		t.Errorf("expected active at Tue 17:00 CT")
	}
	// Tuesday 15:59 CT — one minute before, not active.
	tue1559 := time.Date(2026, time.July, 14, 15, 59, 0, 0, ct)
	if w.IsActive(tue1559) {
		t.Errorf("expected inactive at Tue 15:59 CT")
	}
	// Tuesday 21:00 CT — boundary, end is exclusive, not active.
	tue2100 := time.Date(2026, time.July, 14, 21, 0, 0, 0, ct)
	if w.IsActive(tue2100) {
		t.Errorf("expected inactive at Tue 21:00 CT (end-exclusive)")
	}
	// Saturday 17:00 CT — wrong day of week.
	sat1700 := time.Date(2026, time.July, 11, 17, 0, 0, 0, ct)
	if w.IsActive(sat1700) {
		t.Errorf("expected inactive Sat 17:00 CT (wrong DOW)")
	}
}

func TestWindow_IsActive_TimezoneAware(t *testing.T) {
	// Same window as above but the check time is constructed in UTC.
	// IsActive should convert to CT internally.
	w := Window{
		Name:       "summer peak",
		Timezone:   "America/Chicago",
		StartLocal: "16:00",
		EndLocal:   "21:00",
		DaysOfWeek: []int{1, 2, 3, 4, 5},
	}
	// Tuesday 17:30 CT == 22:30 UTC (CDT is UTC-5).
	checkUTC := time.Date(2026, time.July, 14, 22, 30, 0, 0, time.UTC)
	if !w.IsActive(checkUTC) {
		t.Errorf("expected active at Tue 17:30 CT (22:30 UTC)")
	}
}

func TestWindow_IsActive_WrapMidnight(t *testing.T) {
	// Overnight maintenance freeze: 22:00 to 06:00.
	w := Window{
		Name:       "overnight freeze",
		Timezone:   "UTC",
		StartLocal: "22:00",
		EndLocal:   "06:00",
		DaysOfWeek: []int{1}, // Monday only — the start day
	}
	// Monday 23:00 — inside tonight's slot.
	mon23 := time.Date(2026, time.July, 13, 23, 0, 0, 0, time.UTC)
	if !w.IsActive(mon23) {
		t.Errorf("expected active Mon 23:00")
	}
	// Tuesday 02:00 — inside the wrap tail of yesterday's window.
	tue02 := time.Date(2026, time.July, 14, 2, 0, 0, 0, time.UTC)
	if !w.IsActive(tue02) {
		t.Errorf("expected active Tue 02:00 (wrap tail of Mon window)")
	}
	// Tuesday 07:00 — past the wrap end, not active.
	tue07 := time.Date(2026, time.July, 14, 7, 0, 0, 0, time.UTC)
	if w.IsActive(tue07) {
		t.Errorf("expected inactive Tue 07:00")
	}
}

func TestWindow_IsActive_EffectiveRange(t *testing.T) {
	w := Window{
		Name:          "Q4 freeze",
		Timezone:      "UTC",
		StartLocal:    "00:00",
		EndLocal:      "23:59",
		EffectiveFrom: "2026-11-15T00:00:00Z",
		EffectiveTo:   "2027-01-05T23:59:59Z",
	}
	// During freeze.
	dec25 := time.Date(2026, time.December, 25, 12, 0, 0, 0, time.UTC)
	if !w.IsActive(dec25) {
		t.Errorf("expected active during Q4 freeze")
	}
	// Before freeze.
	oct1 := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	if w.IsActive(oct1) {
		t.Errorf("expected inactive before EffectiveFrom")
	}
	// After freeze.
	feb1 := time.Date(2027, time.February, 1, 12, 0, 0, 0, time.UTC)
	if w.IsActive(feb1) {
		t.Errorf("expected inactive after EffectiveTo")
	}
}

func TestFirstActive(t *testing.T) {
	windows := []Window{
		{Name: "peak", Timezone: "UTC", StartLocal: "16:00", EndLocal: "21:00", DaysOfWeek: []int{2}},
		{Name: "lunch", Timezone: "UTC", StartLocal: "12:00", EndLocal: "13:00", DaysOfWeek: []int{2}},
	}
	// Tuesday 12:30 UTC — second window matches.
	tueLunch := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	got := FirstActive(windows, tueLunch)
	if got == nil || got.Name != "lunch" {
		t.Errorf("expected lunch, got %+v", got)
	}
	// Tuesday 09:00 UTC — none match.
	tueMorning := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	if FirstActive(windows, tueMorning) != nil {
		t.Errorf("expected no active window at Tue 09:00 UTC")
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
