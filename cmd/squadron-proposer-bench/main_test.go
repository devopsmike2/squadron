// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	"github.com/devopsmike2/squadron/internal/ai"
)

// TestClassify pins the outcome bucketing logic — the bench's whole
// value lives in separating "succeeded" / "declined" / "truncated" /
// "parse_failed_preamble" / "parse_failed_other" so a regression in
// one class is visible from the report.
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		result *ai.ProposalResult
		err    error
		want   string
	}{
		{
			name:   "succeeded — rollout-kind, no error",
			result: &ai.ProposalResult{Kind: ai.ProposalKindRollout},
			want:   "succeeded",
		},
		{
			name:   "succeeded — plan-kind",
			result: &ai.ProposalResult{Kind: ai.ProposalKindPlan},
			want:   "succeeded",
		},
		{
			name:   "declined — model said no",
			result: &ai.ProposalResult{Declined: true},
			want:   "declined",
		},
		{
			name: "truncated — #550 signature",
			err:  errors.New("propose from cost spike: model response was not valid JSON: unexpected end of JSON input (raw=)"),
			want: "truncated",
		},
		{
			name: "parse_failed_preamble — #552 signature (model wrote prose first)",
			err:  errors.New("propose from cost spike: model response was not valid JSON: invalid character 'L' looking for beginning of value (raw=Looking at the context:...)"),
			want: "parse_failed_preamble",
		},
		{
			name: "parse_failed_other — invalid character but no preamble signature",
			err:  errors.New("propose from cost spike: model response was not valid JSON: invalid character '}' at offset 5 (raw={})"),
			want: "parse_failed_other",
		},
		{
			name: "llm_error — unrecognized failure",
			err:  errors.New("connection reset by peer"),
			want: "llm_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.result, tc.err)
			if got != tc.want {
				t.Errorf("classify(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPercentile sanity-checks the helper at common indices. Empty
// input is the easy gotcha; the bench passes an empty slice when no
// seeds match the filter regex.
func TestPercentile(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		p    float64
		want int
	}{
		{"empty", nil, 0.5, 0},
		{"single", []int{42}, 0.5, 42},
		{"p50 of 1..10", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.5, 5},
		{"p95 of 1..10", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 9},
		{"p99 of 1..10", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.99, 9},
		{"p100 of 1..10", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 1.0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(tc.in, tc.p)
			if got != tc.want {
				t.Errorf("percentile(%v, %v) = %d, want %d", tc.in, tc.p, got, tc.want)
			}
		})
	}
}

// TestEstimatedUSD sanity-checks the cost-per-call estimator at
// realistic v0.82 token sizes so the bench's "total cost" line stays
// in the right order of magnitude release over release.
func TestEstimatedUSD(t *testing.T) {
	// v0.82 proposer succeeded at 1848 in / 1090 out.
	// in:  1848/1e6 * 3.00 = 0.005544
	// out: 1090/1e6 * 15.00 = 0.01635
	// total ≈ 0.02189
	got := estimatedUSD(1848, 1090)
	want := 0.021894
	if got < want-0.0001 || got > want+0.0001 {
		t.Errorf("estimatedUSD(1848, 1090) = %f, want approximately %f", got, want)
	}
}

// TestSeedKindDispatch verifies the v0.86 bi-modal corpus shape:
// both arcs are present, every seed declares a kind, and discovery
// seeds carry a non-empty scan.AccountID (the discovery proposer's
// validator hard-fails on empty account_id, so an empty value in the
// corpus would manifest as an llm_error / validation rejection at
// run time instead of an obvious data-shape error here).
func TestSeedKindDispatch(t *testing.T) {
	seen := map[seedKind]int{}
	for _, sd := range corpus() {
		if sd.kind == "" {
			t.Errorf("seed %q has empty kind", sd.name)
			continue
		}
		seen[sd.kind]++
		switch sd.kind {
		case seedKindCostSpike:
			if sd.spike.SpikeID == "" {
				t.Errorf("cost_spike seed %q has empty SpikeID", sd.name)
			}
		case seedKindDiscovery:
			if sd.scan.AccountID == "" {
				t.Errorf("discovery seed %q has empty scan.AccountID", sd.name)
			}
			if sd.scan.ScanID == "" {
				t.Errorf("discovery seed %q has empty scan.ScanID", sd.name)
			}
		default:
			t.Errorf("seed %q has unknown kind %q", sd.name, sd.kind)
		}
	}
	if seen[seedKindCostSpike] == 0 {
		t.Error("corpus has no cost_spike seeds; the bench is no longer bi-modal")
	}
	if seen[seedKindDiscovery] == 0 {
		t.Error("corpus has no discovery seeds; the bench is no longer bi-modal")
	}
}

// TestClassify_StillHandlesDeclineForDiscovery is a regression guard:
// the v0.86 discovery arc reuses the same classify() function as the
// cost-spike arc. The "declined" bucket has to keep working when a
// discovery scan returns a ProposalResult with Declined=true (e.g.
// the empty-inventory and fully-instrumented seeds). The function is
// already kind-agnostic — it only reads Declined and err — so this
// test pins that the absence of kind-specific branching is correct.
func TestClassify_StillHandlesDeclineForDiscovery(t *testing.T) {
	// Discovery results carry Kind = ProposalKindPlan when accepted;
	// Declined=true is the empty-inventory / fully-covered outcome.
	res := &ai.ProposalResult{Declined: true, Reason: "no uninstrumented resources"}
	if got := classify(res, nil); got != "declined" {
		t.Errorf("classify(declined discovery result) = %q, want \"declined\"", got)
	}
	// And a plan-kind successful discovery result still classifies
	// as "succeeded" — same as the cost-spike plan-kind path.
	res2 := &ai.ProposalResult{Kind: ai.ProposalKindPlan}
	if got := classify(res2, nil); got != "succeeded" {
		t.Errorf("classify(plan-kind discovery result) = %q, want \"succeeded\"", got)
	}
}

// TestAggregate_ByKindCountsCorrectly feeds a known mix of seed
// results through buildAggregate and pins the ByKind map's sums.
// Catches the two easy mistakes: (1) the per-kind totals double-
// counting, and (2) the failed-bucket count missing one of the
// non-succeeded / non-declined outcome strings.
func TestAggregate_ByKindCountsCorrectly(t *testing.T) {
	results := []seedResult{
		{Seed: "cs1", SeedKind: string(seedKindCostSpike), Outcome: "succeeded"},
		{Seed: "cs2", SeedKind: string(seedKindCostSpike), Outcome: "succeeded"},
		{Seed: "cs3", SeedKind: string(seedKindCostSpike), Outcome: "declined"},
		{Seed: "cs4", SeedKind: string(seedKindCostSpike), Outcome: "truncated"},
		{Seed: "d1", SeedKind: string(seedKindDiscovery), Outcome: "succeeded"},
		{Seed: "d2", SeedKind: string(seedKindDiscovery), Outcome: "declined"},
		{Seed: "d3", SeedKind: string(seedKindDiscovery), Outcome: "declined"},
		{Seed: "d4", SeedKind: string(seedKindDiscovery), Outcome: "parse_failed_preamble"},
		{Seed: "d5", SeedKind: string(seedKindDiscovery), Outcome: "llm_error"},
	}
	agg := buildAggregate(results)

	cs, ok := agg.ByKind[string(seedKindCostSpike)]
	if !ok {
		t.Fatalf("ByKind missing cost_spike entry: %+v", agg.ByKind)
	}
	if cs.Total != 4 || cs.Succeeded != 2 || cs.Declined != 1 || cs.Failed != 1 {
		t.Errorf("cost_spike counts = %+v, want {Total:4 Succeeded:2 Declined:1 Failed:1}", cs)
	}

	dc, ok := agg.ByKind[string(seedKindDiscovery)]
	if !ok {
		t.Fatalf("ByKind missing discovery entry: %+v", agg.ByKind)
	}
	if dc.Total != 5 || dc.Succeeded != 1 || dc.Declined != 2 || dc.Failed != 2 {
		t.Errorf("discovery counts = %+v, want {Total:5 Succeeded:1 Declined:2 Failed:2}", dc)
	}

	// Sanity: ByKind totals reconcile with the top-level Total.
	if cs.Total+dc.Total != agg.Total {
		t.Errorf("ByKind totals (cs=%d + d=%d = %d) != agg.Total (%d)",
			cs.Total, dc.Total, cs.Total+dc.Total, agg.Total)
	}

	// Sanity: succeeded + declined + failed per kind reconciles with
	// per-kind total.
	if cs.Succeeded+cs.Declined+cs.Failed != cs.Total {
		t.Errorf("cost_spike per-bucket sums (%d) != Total (%d)",
			cs.Succeeded+cs.Declined+cs.Failed, cs.Total)
	}
	if dc.Succeeded+dc.Declined+dc.Failed != dc.Total {
		t.Errorf("discovery per-bucket sums (%d) != Total (%d)",
			dc.Succeeded+dc.Declined+dc.Failed, dc.Total)
	}
}

func TestHasPreambleSignature(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"no raw= section", "unexpected end of JSON input", false},
		{"raw starts with letter (preamble)", "invalid character (raw=Looking at the context: foo)", true},
		{"raw starts with brace (no preamble)", "invalid character (raw={})", false},
		{"raw starts with whitespace then letter", "invalid character (raw=   Looking)", true},
		{"raw starts with quoted letter", "invalid character (raw=\"Looking)", true},
		{"raw is empty", "invalid character (raw=)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasPreambleSignature(tc.msg)
			if got != tc.want {
				t.Errorf("hasPreambleSignature(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
