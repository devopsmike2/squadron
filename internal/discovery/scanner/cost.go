// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Cost-correlation substrate slice 6 chunk 1 (v0.89.183, #825 Stream
// 222) — the money-touching plumbing for cost correlation, shipped in
// isolation before any per-cloud billing integration. See
// docs/proposals/cost-correlation-substrate-slice6.md.
//
// HARD GUARDRAILS enforced here:
//
//  1. READ-ONLY. CostQuerier exposes exactly one verb — a cost READ.
//     There is no write/provisioning method. Per-cloud implementations
//     are restricted to the providers' cost-REPORTING APIs and MUST
//     NOT issue any call that incurs cost on the user's behalf.
//
//  2. BOUNDED SPEND. Some cost-reporting APIs charge per request (AWS
//     Cost Explorer ~$0.01/request). Every cost call is gated by a
//     CostBudgetGovernor that caps cumulative per-account spend against
//     a ceiling (default $1/30-day-window) and rejects calls that would
//     exceed it.
//
//  3. REPORT, DON'T EDITORIALIZE. Cost data surfaces as a plain figure;
//     the substrate carries the number + currency only, no "high"/"low"
//     judgement.

// MicroUSD is millionths of a US dollar, the substrate's integer money
// unit. Money is NEVER represented as float here — the governor's
// ceiling check must be exact, and float dollars drift. $0.01 =
// 10_000 MicroUSD; $1.00 = 1_000_000 MicroUSD.
type MicroUSD = int64

const (
	// MicroUSDPerCent is the MicroUSD value of one US cent ($0.01).
	MicroUSDPerCent MicroUSD = 10_000

	// MicroUSDPerDollar is the MicroUSD value of one US dollar.
	MicroUSDPerDollar MicroUSD = 1_000_000

	// DefaultMonthlyCostBudgetMicroUSD is the default per-account
	// spend ceiling the CostBudgetGovernor enforces: $1.00 per rolling
	// window. Sized so the worst-case per-call-priced surface (AWS
	// Cost Explorer at ~$0.01/call) is capped at ~100 calls/account/
	// window — comfortably under any reasonable scan cadence. Operators
	// can raise it explicitly; the default is deliberately conservative.
	DefaultMonthlyCostBudgetMicroUSD MicroUSD = 1 * MicroUSDPerDollar
)

// DefaultCostBudgetWindow is the rolling window the governor accounts
// spend over before resetting. 30 days approximates a monthly billing
// period without coupling to calendar-month boundaries.
const DefaultCostBudgetWindow = 30 * 24 * time.Hour

// CostGranularity is the time granularity of a cost figure.
type CostGranularity string

const (
	// CostGranularityDaily is per-day cost.
	CostGranularityDaily CostGranularity = "daily"
	// CostGranularityMonthly is per-month cost.
	CostGranularityMonthly CostGranularity = "monthly"
)

// CostResult is the return shape from a CostQuerier.QueryCost call.
// Mirrors AggregateMetricResult's denormalized-echo + not-measured
// contract.
//
// Empty-result semantics: a cost query against a resource with no
// billing data in the window returns AmountMicroUSD=0, Covered=false,
// no error. Callers MUST check Covered before reading AmountMicroUSD
// to distinguish "genuinely $0" (Covered=true, Amount=0) from "not
// measured" (Covered=false) — the same distinction the metric
// substrate draws with SampleCount.
type CostResult struct {
	// ResourceID echoes the caller's input — the provider-native
	// resource identifier the cost was attributed to.
	ResourceID string

	// Dimension echoes the caller's input — the cost grouping the
	// query requested (e.g. a service/usage-type filter).
	Dimension string

	// AmountMicroUSD is the attributed cost in micro-USD over the
	// window. Always 0 when Covered is false.
	AmountMicroUSD MicroUSD

	// Currency is the ISO-4217 code the provider reported. The
	// substrate normalizes the magnitude to USD micro-units; Currency
	// records the source currency for audit (non-USD sources are
	// converted by the per-cloud implementation, which stamps "USD").
	Currency string

	// Granularity is the time granularity of the figure.
	Granularity CostGranularity

	// Window is the duration the cost covers. Echoes the caller input.
	Window time.Duration

	// Covered reports whether the provider returned billing data for
	// the resource+window. False = not measured (see type godoc);
	// callers keep the not-measured sentinel rather than reporting $0.
	Covered bool

	// ObservedAt is the reference time of the query.
	ObservedAt time.Time
}

// CostQuerier is the read-only interface per-cloud scanners implement
// to attribute cost to resources. Mirrors MetricQuerier: a single
// read verb, a method on the per-cloud Scanner so the credential chain
// is shared, empty-result semantics, and per-cloud rate/budget limits.
//
// READ-ONLY BY CONSTRUCTION: there is exactly one method and it is a
// read. No per-cloud implementation may add a mutating sibling on this
// interface. Implementations MUST gate every charged request through a
// CostBudgetGovernor before issuing it.
//
// See docs/proposals/cost-correlation-substrate-slice6.md §5.
type CostQuerier interface {
	// QueryCost returns the attributed cost for the supplied resource
	// over the supplied window. dimension is the per-cloud cost
	// grouping/filter; the substrate does not normalize dimensions
	// across clouds — callers hold a concrete per-cloud CostQuerier.
	QueryCost(
		ctx context.Context,
		resourceID string,
		dimension string,
		window time.Duration,
	) (CostResult, error)
}

// ErrCostNotImplemented is the sentinel returned by per-cloud
// CostQuerier skeletons that haven't shipped a concrete body yet.
// Comparable via errors.Is. Chunk 1 ships only the interface +
// skeleton sentinel so downstream chunks can be written against the
// stable shape, mirroring ErrMetricNotImplemented.
var ErrCostNotImplemented = errors.New("cost query not implemented for this provider in cost-correlation slice 6 chunk 1")

// ErrCostBudgetExceeded is returned by CostBudgetGovernor.Authorize
// when a charged cost call would push cumulative per-account spend
// over the ceiling. The caller MUST treat this as a graceful skip —
// leave cost fields at the not-measured sentinel and continue the
// scan — NOT as a fatal error. Comparable via errors.Is.
var ErrCostBudgetExceeded = errors.New("cost query budget exceeded for this account window")

// CostBudgetGovernor caps cumulative per-account spend on charged
// cost-reporting API calls. It is the load-bearing money guardrail:
// no charged cost call may be issued without a successful Authorize.
//
// Thread-safe. Construct one per connected account (alongside the
// per-cloud rate limiters) and share it across the account's cost
// calls so concurrent scans can't collectively overspend.
type CostBudgetGovernor struct {
	mu          sync.Mutex
	ceiling     MicroUSD
	window      time.Duration
	spent       MicroUSD
	windowStart time.Time
	now         func() time.Time
}

// NewCostBudgetGovernor returns a governor with the supplied ceiling
// and window. A non-positive ceiling falls back to
// DefaultMonthlyCostBudgetMicroUSD; a non-positive window falls back to
// DefaultCostBudgetWindow — so a zero-value-ish construction is still
// safe rather than permitting unbounded spend or never resetting.
func NewCostBudgetGovernor(ceiling MicroUSD, window time.Duration) *CostBudgetGovernor {
	if ceiling <= 0 {
		ceiling = DefaultMonthlyCostBudgetMicroUSD
	}
	if window <= 0 {
		window = DefaultCostBudgetWindow
	}
	return &CostBudgetGovernor{
		ceiling: ceiling,
		window:  window,
		now:     time.Now,
	}
}

// Authorize is called immediately before every cost API request with
// that request's per-call cost in micro-USD. It rolls the spend window
// forward if elapsed, then either records the spend and returns nil, or
// returns ErrCostBudgetExceeded WITHOUT recording the spend.
//
// A zero or negative perCallMicroUSD (a free-API surface, e.g. Azure
// Cost Management or OCI usage reports) is always authorized and never
// affects the accumulated spend — the governor only constrains
// surfaces that actually charge.
func (g *CostBudgetGovernor) Authorize(perCallMicroUSD MicroUSD) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if g.windowStart.IsZero() || now.Sub(g.windowStart) >= g.window {
		g.windowStart = now
		g.spent = 0
	}

	if perCallMicroUSD <= 0 {
		// Free surface — always allowed, no spend recorded.
		return nil
	}
	if g.spent+perCallMicroUSD > g.ceiling {
		return ErrCostBudgetExceeded
	}
	g.spent += perCallMicroUSD
	return nil
}

// Spent returns the cumulative recorded spend in the current window.
func (g *CostBudgetGovernor) Spent() MicroUSD {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent
}

// Remaining returns the budget left in the current window (never
// negative).
func (g *CostBudgetGovernor) Remaining() MicroUSD {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.ceiling - g.spent
	if r < 0 {
		r = 0
	}
	return r
}

// Ceiling returns the governor's spend ceiling.
func (g *CostBudgetGovernor) Ceiling() MicroUSD {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ceiling
}
