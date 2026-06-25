// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package verdictsel implements the shared verdict selection policy
// used by both the cost-spike proposer (#531) and the discovery
// proposer (#643) feedback loops. It is a pure functional layer:
// callers from each surface assemble their own []Verdict from
// surface-local storage and pass it to Select; Select returns the
// curated, capped subset to feed into the prompt.
//
// See docs/proposals/531-proposer-learning-slice2.md §3 for the
// architectural decision (shared shape, not shared substrate) and §6
// for the selection algorithm.
package verdictsel

import (
	"sort"
	"time"
)

// State distinguishes the verdict outcomes Select buckets into the
// approved or rejected slice. The string values are stable and
// intended to be persisted (they appear in audit-event payloads).
const (
	StateApproved         = "approved"          // cost-spike: rollout approved
	StateRejected         = "rejected"          // cost-spike: rollout rejected
	StateMerged           = "merged"            // discovery: PR merged
	StateClosedNotMerged  = "closed_not_merged" // discovery: PR closed without merge
	StateOperatorExcluded = "operator_excluded" // discovery: operator clicked Don't propose this again
)

// Verdict is the surface-agnostic shape Select consumes. Both cost-
// spike (rollouts) and discovery (audit events + exclusion table)
// project into this shape at the bridge layer.
type Verdict struct {
	ID        string    // rollout_id OR PR URL OR recommendation_id
	Kind      string    // rollout kind OR recommendation_kind
	State     string    // one of the State* constants above
	Timestamp time.Time // approved_at / rejected_at / merged_at / closed_at / excluded_at
	Body      string    // surface-specific summary (reasoning + notes OR branch + merged_by)
	Excluded  bool      // operator-set; filtered out in step 1 regardless of state
}

// SelectOpts carries the policy knobs Select uses. Callers fill
// these in from package-level Default* constants in consts.go.
type SelectOpts struct {
	Now        time.Time     // injected for determinism in tests
	Window     time.Duration // hard cliff; verdicts older than (Now - Window) dropped
	HotWindow  time.Duration // within Window, verdicts within HotWindow emit first
	MaxTotal   int           // total cap across both buckets after caps applied
	MaxPerKind int           // per-kind cap inside each bucket
	PreferNeg  bool          // true: fill rejected bucket to MaxTotal/2 before approved
}

// Select implements the §6 selection algorithm. The function is
// pure: same inputs always produce the same output slice. Callers
// pass opts.Now explicitly so the algorithm is deterministic in
// tests.
//
// The algorithm in order:
//  1. Filter Excluded=true and Timestamp < Now-Window.
//  2. Bucket by State: approved={Approved,Merged};
//     rejected={Rejected,ClosedNotMerged,OperatorExcluded}.
//  3. Within each bucket, partition into hot/cold tiers (HotWindow
//     boundary). Sort each tier newest-first; ties broken by ID
//     ascending for determinism.
//  4. Walk hot-then-cold within each bucket, applying MaxPerKind to
//     the running per-bucket kind histogram.
//  5. Apply PreferNeg + MaxTotal: PreferNeg=true fills rejected up
//     to MaxTotal/2 first; PreferNeg=false alternates one rejected,
//     one approved.
//  6. Return rejected first, then approved.
func Select(rows []Verdict, opts SelectOpts) []Verdict {
	if len(rows) == 0 {
		return nil
	}

	// Step 1: filter Excluded + recency cliff.
	cutoff := opts.Now.Add(-opts.Window)
	filtered := make([]Verdict, 0, len(rows))
	for _, r := range rows {
		if r.Excluded {
			continue
		}
		if r.Timestamp.Before(cutoff) {
			continue
		}
		filtered = append(filtered, r)
	}
	if len(filtered) == 0 {
		return nil
	}

	// Step 2: bucket by State. Any State string not recognised
	// falls through to neither bucket (defensive).
	var approvedRows, rejectedRows []Verdict
	for _, r := range filtered {
		switch r.State {
		case StateApproved, StateMerged:
			approvedRows = append(approvedRows, r)
		case StateRejected, StateClosedNotMerged, StateOperatorExcluded:
			rejectedRows = append(rejectedRows, r)
		}
	}

	// Steps 3+4: tier within each bucket and apply MaxPerKind.
	approvedPicked := tierAndCap(approvedRows, opts)
	rejectedPicked := tierAndCap(rejectedRows, opts)

	// Step 5: PreferNeg + MaxTotal.
	totalCap := opts.MaxTotal
	if totalCap < 0 {
		totalCap = 0
	}
	var pickedApproved, pickedRejected []Verdict
	if opts.PreferNeg {
		// Fill rejected up to MaxTotal/2 first; then fill approved
		// with the remainder.
		half := totalCap / 2
		if len(rejectedPicked) < half {
			pickedRejected = append(pickedRejected, rejectedPicked...)
		} else {
			pickedRejected = append(pickedRejected, rejectedPicked[:half]...)
		}
		remaining := totalCap - len(pickedRejected)
		if len(approvedPicked) < remaining {
			pickedApproved = append(pickedApproved, approvedPicked...)
		} else if remaining > 0 {
			pickedApproved = append(pickedApproved, approvedPicked[:remaining]...)
		}
	} else {
		// Parallel fill: alternate rejected, approved until either
		// bucket is empty or MaxTotal is reached.
		ri, ai := 0, 0
		for len(pickedRejected)+len(pickedApproved) < totalCap {
			progressed := false
			if ri < len(rejectedPicked) {
				pickedRejected = append(pickedRejected, rejectedPicked[ri])
				ri++
				progressed = true
				if len(pickedRejected)+len(pickedApproved) >= totalCap {
					break
				}
			}
			if ai < len(approvedPicked) {
				pickedApproved = append(pickedApproved, approvedPicked[ai])
				ai++
				progressed = true
			}
			if !progressed {
				break
			}
		}
	}

	// Step 6: rejected first, then approved.
	out := make([]Verdict, 0, len(pickedRejected)+len(pickedApproved))
	out = append(out, pickedRejected...)
	out = append(out, pickedApproved...)
	return out
}

// tierAndCap partitions rows by hot/cold tier, sorts each tier
// newest-first (ID ascending tie-break for determinism), then walks
// hot-then-cold applying MaxPerKind. Helper for Select.
func tierAndCap(rows []Verdict, opts SelectOpts) []Verdict {
	if len(rows) == 0 {
		return nil
	}
	hotCutoff := opts.Now.Add(-opts.HotWindow)
	var hot, cold []Verdict
	for _, r := range rows {
		if !r.Timestamp.Before(hotCutoff) {
			hot = append(hot, r)
		} else {
			cold = append(cold, r)
		}
	}
	sortTier(hot)
	sortTier(cold)

	picked := make([]Verdict, 0, len(rows))
	counts := make(map[string]int)
	walk := func(tier []Verdict) {
		for _, r := range tier {
			if opts.MaxPerKind > 0 && counts[r.Kind] >= opts.MaxPerKind {
				continue
			}
			picked = append(picked, r)
			counts[r.Kind]++
		}
	}
	walk(hot)
	walk(cold)
	return picked
}

// sortTier sorts in-place by Timestamp DESC, ID ASC tie-break.
func sortTier(rows []Verdict) {
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Timestamp.Equal(rows[j].Timestamp) {
			return rows[i].Timestamp.After(rows[j].Timestamp)
		}
		return rows[i].ID < rows[j].ID
	})
}
