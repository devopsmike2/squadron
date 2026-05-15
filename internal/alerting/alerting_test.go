// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package alerting

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/stretchr/testify/assert"
)

// TestCompareThreshold exhaustively walks every operator with values on both
// sides of the threshold. This is the per-tick decision function — if it has
// an off-by-one or operator swap, alerts fire at the wrong threshold which
// erodes trust fast.
func TestCompareThreshold(t *testing.T) {
	cases := []struct {
		name      string
		value     float64
		op        services.ThresholdOperator
		threshold float64
		want      bool
	}{
		// >
		{"gt true", 11, services.ThresholdGreater, 10, true},
		{"gt false equal", 10, services.ThresholdGreater, 10, false},
		{"gt false less", 9, services.ThresholdGreater, 10, false},

		// >=
		{"ge true", 11, services.ThresholdGreaterOrEqual, 10, true},
		{"ge true equal", 10, services.ThresholdGreaterOrEqual, 10, true},
		{"ge false", 9, services.ThresholdGreaterOrEqual, 10, false},

		// <
		{"lt true", 9, services.ThresholdLess, 10, true},
		{"lt false equal", 10, services.ThresholdLess, 10, false},
		{"lt false greater", 11, services.ThresholdLess, 10, false},

		// <=
		{"le true", 9, services.ThresholdLessOrEqual, 10, true},
		{"le true equal", 10, services.ThresholdLessOrEqual, 10, true},
		{"le false", 11, services.ThresholdLessOrEqual, 10, false},

		// ==
		{"eq true", 10, services.ThresholdEqual, 10, true},
		{"eq false low", 9, services.ThresholdEqual, 10, false},
		{"eq false high", 11, services.ThresholdEqual, 10, false},

		// !=
		{"ne true low", 9, services.ThresholdNotEqual, 10, true},
		{"ne true high", 11, services.ThresholdNotEqual, 10, true},
		{"ne false", 10, services.ThresholdNotEqual, 10, false},

		// unknown operator — should default to false rather than fire
		{"unknown op", 999, services.ThresholdOperator("approx"), 10, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compareThreshold(tc.value, tc.op, tc.threshold)
			assert.Equal(t, tc.want, got,
				"compareThreshold(%v, %q, %v) = %v, want %v",
				tc.value, tc.op, tc.threshold, got, tc.want)
		})
	}
}
