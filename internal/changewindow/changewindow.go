// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package changewindow models recurring blackout windows that block
// rollout advancement during change-restricted periods (peak-demand
// hours, storm-response windows, end-of-quarter freezes).
//
// Design notes:
//   - A Window is BLOCK semantics. During an active window, the
//     rollout engine refuses to advance. The simpler primitive — we
//     considered allow-windows but block-windows match how operators
//     actually describe their policies ("no rollouts to prod between
//     16:00 and 21:00 weekdays during summer").
//   - Time math is local to the configured IANA timezone, not UTC.
//     A utility scheduler thinks in their grid's local clock, not
//     UTC; doing the conversion at check time keeps the operator's
//     mental model intact.
//   - Storage shape is JSON on the parent record (Group today) — one
//     column avoids a new table for a piece of config the operator
//     owns end-to-end. The trade-off is no indexing; we never query
//     "which groups have a window active right now" — we always go
//     in the other direction (here's a group, check its windows).
//
// Added in v0.49 for NERC CIP-style change-window enforcement.
package changewindow

import (
	"fmt"
	"strings"
	"time"
)

// Window is one recurring (or one-off) blackout. Semantics: rollouts
// to the parent group MUST NOT advance while a window is active.
//
// Fields:
//   - Name: human label shown in the UI ("summer peak demand",
//     "storm response", "Q4 freeze"). Operators see this on the
//     blackout badge.
//   - DaysOfWeek: 0=Sunday..6=Saturday. Empty means every day.
//   - StartLocal, EndLocal: 24h "HH:MM" in the window's Timezone.
//     If EndLocal <= StartLocal, the window wraps midnight (e.g.
//     "22:00" to "06:00" means 22:00 today through 06:00 tomorrow).
//   - Timezone: IANA name ("America/Chicago"). Required.
//   - EffectiveFrom, EffectiveTo: optional RFC3339 timestamps that
//     bracket the window. Use for one-off blackouts (a quarterly
//     freeze) without needing a separate type.
type Window struct {
	Name          string `json:"name"`
	DaysOfWeek    []int  `json:"days_of_week,omitempty"`
	StartLocal    string `json:"start_local"`
	EndLocal      string `json:"end_local"`
	Timezone      string `json:"timezone"`
	EffectiveFrom string `json:"effective_from,omitempty"`
	EffectiveTo   string `json:"effective_to,omitempty"`
}

// Validate reports any structural problem with the window. Called at
// API-write time so a bad config never reaches the engine.
func (w *Window) Validate() error {
	if strings.TrimSpace(w.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(w.Timezone) == "" {
		return fmt.Errorf("timezone is required (use an IANA name like \"America/Chicago\")")
	}
	if _, err := time.LoadLocation(w.Timezone); err != nil {
		return fmt.Errorf("unknown timezone %q: %w", w.Timezone, err)
	}
	if _, err := parseHM(w.StartLocal); err != nil {
		return fmt.Errorf("start_local: %w", err)
	}
	if _, err := parseHM(w.EndLocal); err != nil {
		return fmt.Errorf("end_local: %w", err)
	}
	for _, d := range w.DaysOfWeek {
		if d < 0 || d > 6 {
			return fmt.Errorf("days_of_week values must be 0..6 (got %d)", d)
		}
	}
	if w.EffectiveFrom != "" {
		if _, err := time.Parse(time.RFC3339, w.EffectiveFrom); err != nil {
			return fmt.Errorf("effective_from: %w", err)
		}
	}
	if w.EffectiveTo != "" {
		if _, err := time.Parse(time.RFC3339, w.EffectiveTo); err != nil {
			return fmt.Errorf("effective_to: %w", err)
		}
	}
	return nil
}

// IsActive reports whether the window is in effect at `now`.
//
// Semantics:
//   - Effective range gate first: if now is outside [EffectiveFrom,
//     EffectiveTo], the window is inactive regardless of the
//     weekly pattern.
//   - Then convert now to the window's timezone and check whether
//     today's weekday matches DaysOfWeek (or DaysOfWeek is empty =
//     every day).
//   - Then check whether the local time-of-day falls inside the
//     [StartLocal, EndLocal] window. Wrap-around windows (EndLocal
//     <= StartLocal) cover the period from StartLocal today through
//     EndLocal tomorrow, so we also consider yesterday's weekday for
//     the tail.
func (w *Window) IsActive(now time.Time) bool {
	if w.EffectiveFrom != "" {
		from, err := time.Parse(time.RFC3339, w.EffectiveFrom)
		if err == nil && now.Before(from) {
			return false
		}
	}
	if w.EffectiveTo != "" {
		to, err := time.Parse(time.RFC3339, w.EffectiveTo)
		if err == nil && now.After(to) {
			return false
		}
	}
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		// Defensive — Validate() should have caught this. Failing
		// open (inactive) is safer than failing closed since an
		// operator who can't roll out during a tz typo has no
		// recovery path.
		return false
	}
	local := now.In(loc)
	start, err := parseHM(w.StartLocal)
	if err != nil {
		return false
	}
	end, err := parseHM(w.EndLocal)
	if err != nil {
		return false
	}

	// Today's weekday slot.
	if dowMatches(w.DaysOfWeek, int(local.Weekday())) {
		todayStart := time.Date(local.Year(), local.Month(), local.Day(), start.h, start.m, 0, 0, loc)
		todayEnd := time.Date(local.Year(), local.Month(), local.Day(), end.h, end.m, 0, 0, loc)
		if end.lt(start) {
			// Wrap window — extends past midnight.
			todayEnd = todayEnd.Add(24 * time.Hour)
		}
		if !local.Before(todayStart) && local.Before(todayEnd) {
			return true
		}
	}

	// For wrap-around windows we also need to check if we're in the
	// tail of yesterday's window (00:00..EndLocal today, given a
	// yesterday with a matching DayOfWeek).
	if end.lt(start) {
		yesterday := local.AddDate(0, 0, -1)
		if dowMatches(w.DaysOfWeek, int(yesterday.Weekday())) {
			tailStart := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(),
				start.h, start.m, 0, 0, loc)
			tailEnd := tailStart.Add(wrapDuration(start, end))
			if !local.Before(tailStart) && local.Before(tailEnd) {
				return true
			}
		}
	}
	return false
}

// FirstActive returns the first Window in `windows` that is currently
// active, or nil if none. Engine uses this to decide whether to
// advance and to populate last_blackout_reason.
func FirstActive(windows []Window, now time.Time) *Window {
	for i := range windows {
		if windows[i].IsActive(now) {
			return &windows[i]
		}
	}
	return nil
}

// ValidateAll runs Validate on every window in the slice. Returns
// the first error.
func ValidateAll(windows []Window) error {
	for i := range windows {
		if err := windows[i].Validate(); err != nil {
			return fmt.Errorf("window %d (%q): %w", i, windows[i].Name, err)
		}
	}
	return nil
}

// --- internal helpers -----------------------------------------------------

type hm struct{ h, m int }

func (a hm) lt(b hm) bool {
	if a.h != b.h {
		return a.h < b.h
	}
	return a.m < b.m
}

func parseHM(s string) (hm, error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' {
		return hm{}, fmt.Errorf("expected HH:MM, got %q", s)
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return hm{}, fmt.Errorf("expected HH:MM, got %q: %w", s, err)
	}
	return hm{h: t.Hour(), m: t.Minute()}, nil
}

func dowMatches(days []int, wantedDow int) bool {
	if len(days) == 0 {
		return true // empty = every day
	}
	for _, d := range days {
		if d == wantedDow {
			return true
		}
	}
	return false
}

// wrapDuration returns the duration covered by a wrap window starting
// at `start` and ending at `end` on the next day. Caller has already
// confirmed end < start.
func wrapDuration(start, end hm) time.Duration {
	mins := (24*60 - (start.h*60 + start.m)) + (end.h*60 + end.m)
	return time.Duration(mins) * time.Minute
}
