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
