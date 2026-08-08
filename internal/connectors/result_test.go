// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizedResultMetrics covers the metrics payload shape and its
// Kind invariant.
func TestNormalizedResultMetrics(t *testing.T) {
	now := time.Now().UTC()
	res := NewMetricsResult([]MetricSeries{{
		Labels:  map[string]string{"service": "checkout"},
		Samples: []Sample{{Timestamp: now, Value: 1.5}},
	}})
	require.NoError(t, res.Validate())
	assert.Equal(t, ResultMetrics, res.Kind)
	assert.Nil(t, res.Logs)
	assert.Nil(t, res.Scalar)
	require.Len(t, res.Series, 1)
	assert.Equal(t, 1.5, res.Series[0].Samples[0].Value)
}

// TestNormalizedResultLogs covers the logs payload shape.
func TestNormalizedResultLogs(t *testing.T) {
	res := NewLogsResult([]LogRow{{Timestamp: time.Now().UTC(), Line: "boom"}})
	require.NoError(t, res.Validate())
	assert.Equal(t, ResultLogs, res.Kind)
	assert.Nil(t, res.Series)
	assert.Nil(t, res.Scalar)
	require.Len(t, res.Logs, 1)
}

// TestScalarSLOResult is the load-bearing test for ADR 0034: the result
// model must be able to carry a scalar SLO / burn-rate value with the
// metadata a rollout auto-abort threshold would need, even though the
// auto-abort hook itself is a later slice.
func TestScalarSLOResult(t *testing.T) {
	res := mockBurnRateResult()
	require.NoError(t, res.Validate())
	assert.Equal(t, ResultScalar, res.Kind)
	assert.Nil(t, res.Series)
	assert.Nil(t, res.Logs)

	s := res.Scalar
	require.NotNil(t, s)
	assert.Equal(t, ScalarBurnRate, s.Kind)
	assert.Equal(t, 14.4, s.Value)
	assert.Equal(t, "checkout-availability", s.Objective)
	assert.Equal(t, 0.999, s.SLOTarget)
	assert.Equal(t, 14.4, s.Threshold)
	assert.True(t, s.Breached, "a burn rate at/over threshold must report Breached for the auto-abort consumer")
	assert.False(t, s.Window.Start.IsZero())
	assert.False(t, s.EvaluatedAt.IsZero())
}

// TestNormalizedResultValidateRejectsMismatch asserts the Kind
// discriminator invariant: a Kind with the wrong (or a nil) payload is
// rejected so consumers can switch on Kind safely.
func TestNormalizedResultValidateRejectsMismatch(t *testing.T) {
	t.Run("scalar kind without scalar payload", func(t *testing.T) {
		res := NormalizedResult{Kind: ResultScalar}
		assert.ErrorIs(t, res.Validate(), ErrResultKindMismatch)
	})

	t.Run("metrics kind carrying logs", func(t *testing.T) {
		res := NormalizedResult{Kind: ResultMetrics, Logs: []LogRow{{Line: "x"}}}
		assert.ErrorIs(t, res.Validate(), ErrResultKindMismatch)
	})

	t.Run("scalar kind carrying series", func(t *testing.T) {
		res := NormalizedResult{Kind: ResultScalar, Scalar: &ScalarResult{}, Series: []MetricSeries{{}}}
		assert.ErrorIs(t, res.Validate(), ErrResultKindMismatch)
	})

	t.Run("unknown kind", func(t *testing.T) {
		res := NormalizedResult{Kind: ResultKind("bogus")}
		assert.ErrorIs(t, res.Validate(), ErrResultKindMismatch)
	})
}

// TestTimeRangeDuration covers the small TimeRange helper.
func TestTimeRangeDuration(t *testing.T) {
	now := time.Now().UTC()
	tr := TimeRange{Start: now.Add(-2 * time.Hour), End: now}
	assert.Equal(t, 2*time.Hour, tr.Duration())
}
