// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Cost-correlation substrate slice 6 chunk 1 (v0.89.183, #825 Stream
// 222) — acceptance tests per
// docs/proposals/cost-correlation-substrate-slice6.md §8.

// --- money unit pins --------------------------------------------

func TestMicroUSDUnits(t *testing.T) {
	assert.Equal(t, MicroUSD(10_000), MicroUSDPerCent, "$0.01 = 10_000 micro-USD")
	assert.Equal(t, MicroUSD(1_000_000), MicroUSDPerDollar, "$1.00 = 1_000_000 micro-USD")
	assert.Equal(t, MicroUSD(1_000_000), DefaultMonthlyCostBudgetMicroUSD, "default ceiling is $1.00")
}

// --- §8.1 governor: under ceiling records, over ceiling rejects --

func TestCostBudgetGovernor_AuthorizesUnderCeiling(t *testing.T) {
	g := NewCostBudgetGovernor(DefaultMonthlyCostBudgetMicroUSD, DefaultCostBudgetWindow)
	// $1 ceiling, $0.01/call → 100 calls fit exactly.
	for i := 0; i < 100; i++ {
		assert.NoError(t, g.Authorize(MicroUSDPerCent), "call %d should fit under $1 ceiling", i)
	}
	assert.Equal(t, MicroUSDPerDollar, g.Spent(), "exactly $1 spent")
	assert.Equal(t, MicroUSD(0), g.Remaining())
}

func TestCostBudgetGovernor_RejectsOverCeiling_WithoutRecording(t *testing.T) {
	g := NewCostBudgetGovernor(MicroUSDPerCent, DefaultCostBudgetWindow) // $0.01 ceiling
	assert.NoError(t, g.Authorize(MicroUSDPerCent), "first $0.01 call fits exactly")
	// Second call would exceed.
	err := g.Authorize(MicroUSDPerCent)
	assert.ErrorIs(t, err, ErrCostBudgetExceeded)
	// The rejected spend must NOT be recorded.
	assert.Equal(t, MicroUSDPerCent, g.Spent(), "rejected call does not accumulate spend")
}

// --- free surfaces ($0 calls) are always authorized -------------

func TestCostBudgetGovernor_FreeCallsAlwaysAuthorized(t *testing.T) {
	g := NewCostBudgetGovernor(MicroUSDPerCent, DefaultCostBudgetWindow)
	// Exhaust the ceiling first.
	assert.NoError(t, g.Authorize(MicroUSDPerCent))
	assert.ErrorIs(t, g.Authorize(MicroUSDPerCent), ErrCostBudgetExceeded)
	// A free ($0) call still goes through (Azure / OCI surfaces).
	assert.NoError(t, g.Authorize(0), "free-API call authorized even at ceiling")
	assert.NoError(t, g.Authorize(-5), "non-positive cost treated as free")
	assert.Equal(t, MicroUSDPerCent, g.Spent(), "free calls do not change spend")
}

// --- §8.2 window reset ------------------------------------------

func TestCostBudgetGovernor_ResetsAfterWindow(t *testing.T) {
	g := NewCostBudgetGovernor(MicroUSDPerCent, time.Hour)
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }

	assert.NoError(t, g.Authorize(MicroUSDPerCent))
	assert.ErrorIs(t, g.Authorize(MicroUSDPerCent), ErrCostBudgetExceeded)

	// Advance past the window — spend resets.
	now = now.Add(time.Hour + time.Minute)
	assert.NoError(t, g.Authorize(MicroUSDPerCent), "new window: budget reset")
	assert.Equal(t, MicroUSDPerCent, g.Spent())
}

// --- §8.3 concurrency safety (no double-spend) ------------------

func TestCostBudgetGovernor_ConcurrentNoOverspend(t *testing.T) {
	// $1 ceiling, $0.01/call → at most 100 successes no matter how
	// many goroutines race.
	g := NewCostBudgetGovernor(MicroUSDPerDollar, DefaultCostBudgetWindow)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.Authorize(MicroUSDPerCent) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 100, ok, "exactly 100 calls authorized; no double-spend under concurrency")
	assert.LessOrEqual(t, g.Spent(), MicroUSDPerDollar, "spend never exceeds the ceiling")
}

// --- safe construction fallbacks --------------------------------

func TestNewCostBudgetGovernor_NonPositiveFallbacks(t *testing.T) {
	g := NewCostBudgetGovernor(0, 0) // both non-positive
	assert.Equal(t, DefaultMonthlyCostBudgetMicroUSD, g.Ceiling(), "zero ceiling → safe default, not unbounded")
	// A negative ceiling also falls back rather than permitting infinite spend.
	g2 := NewCostBudgetGovernor(-100, -time.Hour)
	assert.Equal(t, DefaultMonthlyCostBudgetMicroUSD, g2.Ceiling())
}

// --- §8.4 sentinels + skeleton ----------------------------------

func TestCostSentinels_ErrorsIsComparable(t *testing.T) {
	assert.True(t, errors.Is(ErrCostNotImplemented, ErrCostNotImplemented))
	assert.True(t, errors.Is(ErrCostBudgetExceeded, ErrCostBudgetExceeded))
	assert.False(t, errors.Is(ErrCostNotImplemented, ErrCostBudgetExceeded))
}

// skeletonCostQuerier is a stand-in proving the interface is
// satisfiable + the not-implemented sentinel flows (the per-cloud
// bodies land in later chunks).
type skeletonCostQuerier struct{}

func (skeletonCostQuerier) QueryCost(_ context.Context, resourceID, dimension string, window time.Duration) (CostResult, error) {
	return CostResult{ResourceID: resourceID, Dimension: dimension, Window: window}, ErrCostNotImplemented
}

func TestCostQuerier_SkeletonReturnsNotImplemented(t *testing.T) {
	var q CostQuerier = skeletonCostQuerier{}
	res, err := q.QueryCost(context.Background(), "arn:aws:sqs:...:dlq", "service=SQS", time.Hour)
	assert.ErrorIs(t, err, ErrCostNotImplemented)
	assert.Equal(t, "arn:aws:sqs:...:dlq", res.ResourceID, "skeleton echoes request fields")
	assert.False(t, res.Covered, "not-measured by default")
}

// TestNewCostBudgetGovernorFromUSD_ExactMicroUSD: dollars->micro-USD must use
// exact arithmetic, not float truncation. Before the fix, MicroUSD(0.33*1e6)
// yielded 329999 (0.33*1e6 == 329999.9999... in float64), silently gating one
// $0.01 cost query short of the operator's intended budget.
func TestNewCostBudgetGovernorFromUSD_ExactMicroUSD(t *testing.T) {
	cases := map[float64]MicroUSD{
		0.33: 330000,  // the truncation-prone case (was 329999)
		1.10: 1100000, // another inexact float product
		0.01: 10000,   // one cost query's worth
		5.0:  5000000, // whole dollars
		2.50: 2500000, // exact half
	}
	for usd, want := range cases {
		g := NewCostBudgetGovernorFromUSD(usd)
		assert.Equal(t, want, g.Ceiling(), "budget $%.2f should map to %d micro-USD", usd, want)
	}

	// The $0.33 budget must authorize exactly 33 sequential $0.01 queries and
	// reject the 34th — the operator-visible symptom of the truncation bug.
	g := NewCostBudgetGovernorFromUSD(0.33)
	const perCall MicroUSD = 10000 // $0.01
	for i := 0; i < 33; i++ {
		assert.NoErrorf(t, g.Authorize(perCall), "call %d of 33 must be authorized", i+1)
	}
	assert.Error(t, g.Authorize(perCall), "the 34th $0.01 call must exceed the $0.33 budget")
}
