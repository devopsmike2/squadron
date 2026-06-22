// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package verdictsel

import (
	"reflect"
	"testing"
	"time"
)

// fixedNow is a deterministic anchor for all timestamp arithmetic
// in this test file. Picked to be far enough from epoch that any
// relative subtraction stays in a sensible range.
var fixedNow = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

func defaultOpts() SelectOpts {
	return SelectOpts{
		Now:        fixedNow,
		Window:     DefaultWindow,
		HotWindow:  DefaultHotWindow,
		MaxTotal:   DefaultMaxTotal,
		MaxPerKind: DefaultMaxPerKind,
		PreferNeg:  false,
	}
}

// daysAgo returns the time N days before fixedNow.
func daysAgo(n int) time.Time {
	return fixedNow.Add(-time.Duration(n) * 24 * time.Hour)
}

func TestSelect_ColdStart_EmptyInputEmptyOutput(t *testing.T) {
	out := Select(nil, defaultOpts())
	if out != nil {
		t.Fatalf("Select(nil) = %v, want nil", out)
	}
	out = Select([]Verdict{}, defaultOpts())
	if len(out) != 0 {
		t.Fatalf("Select([]) = %v, want empty", out)
	}
}

func TestSelect_TierOrdering_HotBeforeCold_WithinBucket(t *testing.T) {
	rows := []Verdict{
		{ID: "old", Kind: "k1", State: StateApproved, Timestamp: daysAgo(20)},
		{ID: "new", Kind: "k2", State: StateApproved, Timestamp: daysAgo(2)},
	}
	out := Select(rows, defaultOpts())
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2; got %v", len(out), out)
	}
	if out[0].ID != "new" {
		t.Fatalf("expected hot tier ('new') first, got %q", out[0].ID)
	}
	if out[1].ID != "old" {
		t.Fatalf("expected cold tier ('old') second, got %q", out[1].ID)
	}
}

func TestSelect_KindCap_DroppedAtThird(t *testing.T) {
	// 5 approved verdicts, all same kind, all in hot tier.
	// MaxPerKind=2 should keep only the 2 newest.
	rows := []Verdict{
		{ID: "a1", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(1)},
		{ID: "a2", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(2)},
		{ID: "a3", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(3)},
		{ID: "a4", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(4)},
		{ID: "a5", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(5)},
	}
	out := Select(rows, defaultOpts())
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (MaxPerKind=2); got %v", len(out), out)
	}
	if out[0].ID != "a1" || out[1].ID != "a2" {
		t.Fatalf("expected [a1,a2], got [%s,%s]", out[0].ID, out[1].ID)
	}
}

func TestSelect_KindCap_WithMixedKinds_FillsRemainingSlots(t *testing.T) {
	rows := []Verdict{
		// 5 rds-pi-em, all hot.
		{ID: "r1", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(1)},
		{ID: "r2", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(2)},
		{ID: "r3", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(3)},
		{ID: "r4", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(4)},
		{ID: "r5", Kind: "rds-pi-em", State: StateApproved, Timestamp: daysAgo(5)},
		// 1 eks-addon, hot.
		{ID: "e1", Kind: "eks-addon", State: StateApproved, Timestamp: daysAgo(3)},
		// 1 alb-az, hot.
		{ID: "b1", Kind: "alb-az", State: StateApproved, Timestamp: daysAgo(6)},
	}
	out := Select(rows, defaultOpts())
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4; got %v", len(out), out)
	}
	// Walking hot tier newest-first: r1 (1d), r2 (2d), r3 (3d) skipped due to cap,
	// e1 (3d), r4 skipped, r5 skipped, b1 (6d).
	// Tie-break: r3 vs e1 both at 3d → r3 first by ID asc; but r3 is capped.
	wantIDs := []string{"r1", "r2", "e1", "b1"}
	for i, id := range wantIDs {
		if out[i].ID != id {
			t.Fatalf("index %d: got %s, want %s; full = %v", i, out[i].ID, id, ids(out))
		}
	}
}

func TestSelect_PreferNeg_FillsRejectedFirst(t *testing.T) {
	rows := []Verdict{
		{ID: "a1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(1)},
		{ID: "a2", Kind: "k2", State: StateApproved, Timestamp: daysAgo(2)},
		{ID: "a3", Kind: "k3", State: StateApproved, Timestamp: daysAgo(3)},
		{ID: "r1", Kind: "k4", State: StateRejected, Timestamp: daysAgo(1)},
		{ID: "r2", Kind: "k5", State: StateRejected, Timestamp: daysAgo(2)},
		{ID: "r3", Kind: "k6", State: StateRejected, Timestamp: daysAgo(3)},
	}
	opts := defaultOpts()
	opts.PreferNeg = true
	out := Select(rows, opts)
	if len(out) != 4 {
		t.Fatalf("PreferNeg: len = %d, want 4; got %v", len(out), ids(out))
	}
	// PreferNeg=true: 2 rejected (MaxTotal/2) + 2 approved.
	wantPrefer := []string{"r1", "r2", "a1", "a2"}
	for i, id := range wantPrefer {
		if out[i].ID != id {
			t.Fatalf("PreferNeg idx %d: got %s, want %s; full = %v", i, out[i].ID, id, ids(out))
		}
	}

	// Parallel fill: same totals (2+2) but order alternates as
	// rejected, approved, rejected, approved internally — output
	// still emits rejected-block first then approved-block.
	opts.PreferNeg = false
	out2 := Select(rows, opts)
	if len(out2) != 4 {
		t.Fatalf("parallel: len = %d, want 4; got %v", len(out2), ids(out2))
	}
	wantParallel := []string{"r1", "r2", "a1", "a2"}
	for i, id := range wantParallel {
		if out2[i].ID != id {
			t.Fatalf("parallel idx %d: got %s, want %s; full = %v", i, out2[i].ID, id, ids(out2))
		}
	}
}

func TestSelect_PreferNeg_OneRejectedOnly_FillsRemainderWithApproved(t *testing.T) {
	rows := []Verdict{
		{ID: "r1", Kind: "k1", State: StateRejected, Timestamp: daysAgo(1)},
		{ID: "a1", Kind: "k2", State: StateApproved, Timestamp: daysAgo(1)},
		{ID: "a2", Kind: "k3", State: StateApproved, Timestamp: daysAgo(2)},
		{ID: "a3", Kind: "k4", State: StateApproved, Timestamp: daysAgo(3)},
		{ID: "a4", Kind: "k5", State: StateApproved, Timestamp: daysAgo(4)},
		{ID: "a5", Kind: "k6", State: StateApproved, Timestamp: daysAgo(5)},
	}
	opts := defaultOpts()
	opts.PreferNeg = true
	out := Select(rows, opts)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4; got %v", len(out), ids(out))
	}
	// 1 rejected + 3 approved (rejected exhausted, MaxTotal=4 fills with approved).
	want := []string{"r1", "a1", "a2", "a3"}
	for i, id := range want {
		if out[i].ID != id {
			t.Fatalf("idx %d: got %s, want %s; full = %v", i, out[i].ID, id, ids(out))
		}
	}
}

func TestSelect_ExcludedFilter_RemovesVerdict(t *testing.T) {
	rows := []Verdict{
		{ID: "a1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(1)},
		{ID: "a2", Kind: "k2", State: StateApproved, Timestamp: daysAgo(2), Excluded: true},
		{ID: "a3", Kind: "k3", State: StateApproved, Timestamp: daysAgo(3)},
	}
	out := Select(rows, defaultOpts())
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (excluded one filtered); got %v", len(out), ids(out))
	}
	for _, v := range out {
		if v.ID == "a2" {
			t.Fatalf("excluded verdict a2 leaked into output: %v", ids(out))
		}
	}
}

func TestSelect_RecencyWindow_DropsOldVerdict(t *testing.T) {
	rows := []Verdict{
		{ID: "stale", Kind: "k1", State: StateApproved, Timestamp: daysAgo(31)},
	}
	out := Select(rows, defaultOpts())
	if len(out) != 0 {
		t.Fatalf("expected empty (verdict 31d > 30d window), got %v", ids(out))
	}
}

func TestSelect_Determinism_SameInputSameOutput(t *testing.T) {
	rows := []Verdict{
		{ID: "a1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(1), Body: "alpha"},
		{ID: "a2", Kind: "k1", State: StateApproved, Timestamp: daysAgo(2), Body: "beta"},
		{ID: "r1", Kind: "k2", State: StateRejected, Timestamp: daysAgo(1), Body: "gamma"},
		{ID: "r2", Kind: "k2", State: StateRejected, Timestamp: daysAgo(3), Body: "delta"},
		{ID: "m1", Kind: "k3", State: StateMerged, Timestamp: daysAgo(2), Body: "epsilon"},
		{ID: "x1", Kind: "k4", State: StateOperatorExcluded, Timestamp: daysAgo(4), Body: "zeta"},
	}
	opts := defaultOpts()
	opts.PreferNeg = true
	out1 := Select(rows, opts)
	out2 := Select(rows, opts)
	if !reflect.DeepEqual(out1, out2) {
		t.Fatalf("non-deterministic: out1=%v, out2=%v", ids(out1), ids(out2))
	}
}

// TestSelect_GoldenRegression_CuratedTwentyRowInput pins the
// algorithm's behaviour on a curated 20-row corpus spanning all 5
// states, both tiers, and 5 kinds. The expected output is exactly 4
// verdicts under PreferNeg=true / MaxTotal=4 / MaxPerKind=2.
//
// Why these 4 specifically:
//   - PreferNeg=true reserves MaxTotal/2=2 slots for the rejected
//     bucket. The rejected bucket's hot tier walked newest-first
//     with MaxPerKind=2 yields R1 (rejected, k1, 1d) then R2
//     (closed_not_merged, k2, 2d). R3 (rejected, k1, 3d) is then
//     capped by k1; OX1 (operator_excluded, k3, 4d) is hot but
//     trimmed by the half-cap.
//   - The remaining 2 slots fill from the approved bucket: hot tier
//     newest-first → A1 (approved, k1, 1d), A2 (merged, k2, 2d).
//     A3 (approved, k1, 3d) is then capped by k1.
//   - Output ordering: rejected block (R1, R2) then approved block
//     (A1, A2).
//
// Excluded=true rows are dropped in step 1. The 31-day row is
// dropped by the recency window. Cold-tier rows never reach the
// output because the hot tier already fills the bucket caps.
func TestSelect_GoldenRegression_CuratedTwentyRowInput(t *testing.T) {
	rows := []Verdict{
		// --- Rejected bucket, hot tier (0-7d) ---
		{ID: "R1", Kind: "k1", State: StateRejected, Timestamp: daysAgo(1), Body: "r1-body"},
		{ID: "R2", Kind: "k2", State: StateClosedNotMerged, Timestamp: daysAgo(2), Body: "r2-body"},
		{ID: "R3", Kind: "k1", State: StateRejected, Timestamp: daysAgo(3), Body: "r3-body"},
		{ID: "OX1", Kind: "k3", State: StateOperatorExcluded, Timestamp: daysAgo(4), Body: "ox1-body"},
		// --- Rejected bucket, cold tier (7-30d) ---
		{ID: "R4", Kind: "k4", State: StateRejected, Timestamp: daysAgo(10), Body: "r4-body"},
		{ID: "R5", Kind: "k5", State: StateClosedNotMerged, Timestamp: daysAgo(20), Body: "r5-body"},
		// --- Approved bucket, hot tier (0-7d) ---
		{ID: "A1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(1), Body: "a1-body"},
		{ID: "A2", Kind: "k2", State: StateMerged, Timestamp: daysAgo(2), Body: "a2-body"},
		{ID: "A3", Kind: "k1", State: StateApproved, Timestamp: daysAgo(3), Body: "a3-body"},
		{ID: "A4", Kind: "k3", State: StateApproved, Timestamp: daysAgo(5), Body: "a4-body"},
		// --- Approved bucket, cold tier (7-30d) ---
		{ID: "A5", Kind: "k4", State: StateMerged, Timestamp: daysAgo(12), Body: "a5-body"},
		{ID: "A6", Kind: "k5", State: StateApproved, Timestamp: daysAgo(25), Body: "a6-body"},
		// --- Excluded=true, dropped in step 1 ---
		{ID: "E1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(1), Body: "e1-body", Excluded: true},
		{ID: "E2", Kind: "k2", State: StateRejected, Timestamp: daysAgo(1), Body: "e2-body", Excluded: true},
		// --- Past recency window (>30d), dropped in step 1 ---
		{ID: "OLD1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(31), Body: "old1"},
		{ID: "OLD2", Kind: "k2", State: StateRejected, Timestamp: daysAgo(45), Body: "old2"},
		// --- Padding rows to reach 20, mid-cold tier ---
		{ID: "P1", Kind: "k3", State: StateApproved, Timestamp: daysAgo(15), Body: "p1"},
		{ID: "P2", Kind: "k4", State: StateRejected, Timestamp: daysAgo(22), Body: "p2"},
		{ID: "P3", Kind: "k5", State: StateClosedNotMerged, Timestamp: daysAgo(18), Body: "p3"},
		{ID: "P4", Kind: "k3", State: StateMerged, Timestamp: daysAgo(28), Body: "p4"},
	}
	opts := defaultOpts()
	opts.PreferNeg = true

	got := Select(rows, opts)

	want := []Verdict{
		{ID: "R1", Kind: "k1", State: StateRejected, Timestamp: daysAgo(1), Body: "r1-body"},
		{ID: "R2", Kind: "k2", State: StateClosedNotMerged, Timestamp: daysAgo(2), Body: "r2-body"},
		{ID: "A1", Kind: "k1", State: StateApproved, Timestamp: daysAgo(1), Body: "a1-body"},
		{ID: "A2", Kind: "k2", State: StateMerged, Timestamp: daysAgo(2), Body: "a2-body"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("golden mismatch:\n got: %v\nwant: %v", ids(got), ids(want))
	}
}

// ids is a small helper for readable test failure messages.
func ids(rows []Verdict) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
