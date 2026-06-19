// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"testing"
)

// TestEstimateProposerCostUSD pins the cost math against the
// numbers the v0.83 bench uses so the playground's "estimated_usd"
// line stays consistent with the bench's "estimated_usd" line.
// Sonnet 4.6 pricing as of v0.82: $3/MTok input, $15/MTok output.
//
// If Anthropic changes pricing, update this test, the helper above,
// and cmd/squadron-proposer-bench/main.go's constants in lockstep.
// Drifting one of the three is the silent regression this test
// catches.
func TestEstimateProposerCostUSD(t *testing.T) {
	cases := []struct {
		name     string
		in, out  int
		want     float64
		tolerance float64
	}{
		// v0.82 successful proposer call sized: 1848 in / 1090 out.
		// in:  1848/1e6 * 3.00 = 0.005544
		// out: 1090/1e6 * 15.00 = 0.016350
		// total ≈ 0.021894
		{"v0.82 baseline", 1848, 1090, 0.021894, 0.0001},
		// Zero tokens — should be zero. Defensive test against an
		// empty result somehow flowing through with non-zero cost.
		{"zero tokens", 0, 0, 0.0, 1e-9},
		// Output-only — input is free; cost is all on output side.
		{"output only", 0, 1000, 0.015, 1e-9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateProposerCostUSD(tc.in, tc.out)
			if got < tc.want-tc.tolerance || got > tc.want+tc.tolerance {
				t.Errorf("estimateProposerCostUSD(%d, %d) = %f, want approximately %f",
					tc.in, tc.out, got, tc.want)
			}
		})
	}
}
