// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Cost-correlation substrate slice 6 chunk 2 (v0.89.184, #826 Stream
// 223) — the AWS Cost Explorer QueryCost body. First per-cloud
// implementation of the read-only CostQuerier substrate (slice 6
// chunk 1, v0.89.183). AWS is first because Cost Explorer is the one
// cost-reporting surface that materially charges per request, so it's
// the surface that most exercises the spend-bounding governor.
//
// READ-ONLY: this file issues exactly one AWS API — Cost Explorer
// GetCostAndUsage, a read. No mutating / provisioning call.
//
// NOT WIRED INTO ANY SCAN in this chunk: QueryCost is implemented +
// tested but no scan flow calls it yet, so no charged Cost Explorer
// request is made during a scan until the cost-correlation enrichment
// chunk explicitly wires + enables it. This mirrors how the cold-start
// chunk 2 shipped QueryAggregate before the enrichment branch consumed
// it.

// AWSCostExplorerGetCostAndUsageMicroUSD is the per-call cost of the
// Cost Explorer GetCostAndUsage API: AWS charges ~$0.01 per paginated
// request. Pinned to scanner.MicroUSDPerCent ($0.01). The governor is
// authorized for exactly this amount before every GetCostAndUsage
// call. See docs/proposals/cost-correlation-substrate-slice6.md §3 for
// the per-call-cost surface table.
const AWSCostExplorerGetCostAndUsageMicroUSD = scanner.MicroUSDPerCent

// awsCostMetricUnblended is the Cost Explorer metric the substrate
// reads. UnblendedCost is the actual cost charged to the account (vs.
// AmortizedCost / BlendedCost) — the figure an operator sees on their
// bill, which is the honest number to report.
const awsCostMetricUnblended = "UnblendedCost"

// awsCostDateLayout is the date-only format Cost Explorer's
// DateInterval requires (start inclusive, end exclusive).
const awsCostDateLayout = "2006-01-02"

// CostExplorerClient is the minimal surface the AWS CostQuerier needs
// from the Cost Explorer SDK. Slice 6 chunk 2 consumes only
// GetCostAndUsage — the rest of the API is intentionally outside the
// interface so the test fake stays a single-method shape, mirroring
// CloudWatchClient.
//
// READ-ONLY BY CONSTRUCTION: the only method is a read. No per-cloud
// implementation may add a mutating sibling.
type CostExplorerClient interface {
	GetCostAndUsage(
		ctx context.Context,
		in *costexplorer.GetCostAndUsageInput,
		optFns ...func(*costexplorer.Options),
	) (*costexplorer.GetCostAndUsageOutput, error)
}

// QueryCost implements scanner.CostQuerier for AWS via Cost Explorer
// GetCostAndUsage. Reads the UnblendedCost attributed to the supplied
// SERVICE dimension over the window.
//
// dimension is the Cost Explorer SERVICE value to filter on (e.g.
// "Amazon Simple Queue Service"). resourceID echoes onto the result —
// Cost Explorer attributes cost at the service level by default
// (resource-level granularity is a paid opt-in that the substrate
// deliberately avoids), so resourceID is the caller's identifier for
// the resource the service cost is being correlated to.
//
// SPEND GUARDRAIL: a charged GetCostAndUsage call is issued ONLY after
// the per-account CostBudgetGovernor authorizes the per-call cost. The
// charged call is refused (scanner.ErrCostNotImplemented) when either
// the client or the governor is nil — a charged request can never be
// made without spend accounting. When the governor is wired but the
// budget is exhausted, the call returns scanner.ErrCostBudgetExceeded
// with a not-Covered result; the caller treats that as a graceful skip
// (leave cost not-measured) rather than a fatal error.
//
// Empty-result semantics: a service with no billing rows in the window
// returns Covered=false / Amount=0 / no error. Callers MUST check
// Covered before reading AmountMicroUSD.
//
// See docs/proposals/cost-correlation-substrate-slice6.md §3-§5.
func (s *Scanner) QueryCost(
	ctx context.Context,
	resourceID string,
	dimension string,
	window time.Duration,
) (scanner.CostResult, error) {
	base := scanner.CostResult{
		ResourceID:  resourceID,
		Dimension:   dimension,
		Granularity: scanner.CostGranularityMonthly,
		Window:      window,
	}

	// A charged Cost Explorer call requires BOTH a client AND a
	// governor — refuse otherwise so no charged request escapes spend
	// accounting.
	if s.costExplorerClient == nil || s.costGovernor == nil {
		return base, scanner.ErrCostNotImplemented
	}

	// Authorize the per-call spend BEFORE issuing the request.
	if err := s.costGovernor.Authorize(AWSCostExplorerGetCostAndUsageMicroUSD); err != nil {
		// Budget exhausted (or other governor refusal) — graceful skip.
		return base, err
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)
	input := &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.GranularityMonthly,
		Metrics:     []string{awsCostMetricUnblended},
		TimePeriod: &cetypes.DateInterval{
			Start: awssdk.String(startTime.Format(awsCostDateLayout)),
			End:   awssdk.String(endTime.Format(awsCostDateLayout)),
		},
		Filter: &cetypes.Expression{
			Dimensions: &cetypes.DimensionValues{
				Key:    cetypes.DimensionService,
				Values: []string{dimension},
			},
		},
	}

	out, callErr := s.costExplorerClient.GetCostAndUsage(ctx, input)
	if callErr != nil {
		return base, fmt.Errorf("cost explorer get cost and usage: %w", callErr)
	}

	// Sum UnblendedCost across the returned monthly buckets, in
	// integer micro-USD (no float). Covered flips true as soon as a
	// bucket carries a parseable amount.
	var totalMicro scanner.MicroUSD
	currency := ""
	covered := false
	for _, rbt := range out.ResultsByTime {
		mv, ok := rbt.Total[awsCostMetricUnblended]
		if !ok || mv.Amount == nil {
			continue
		}
		micro, perr := parseUSDToMicroUSD(*mv.Amount)
		if perr != nil {
			continue
		}
		covered = true
		totalMicro += micro
		if currency == "" && mv.Unit != nil {
			currency = *mv.Unit
		}
	}

	base.Covered = covered
	base.AmountMicroUSD = totalMicro
	base.Currency = currency
	base.ObservedAt = endTime
	return base, nil
}

// parseUSDToMicroUSD parses a Cost Explorer decimal amount string
// (e.g. "12.3456789") into integer micro-USD WITHOUT float, so the
// governor's accounting and the reported figure stay exact. The
// fractional part is truncated to 6 digits (micro precision); a
// missing fractional part is treated as zero. Leading "-" is honored.
//
// Returns an error on a non-numeric input (so the caller skips the
// bucket rather than recording a bogus 0).
//
// NOTE: Cost Explorer reports in the account's billing currency
// (almost always USD; the Unit is preserved on CostResult.Currency).
// Non-USD FX conversion is deferred — the magnitude is parsed verbatim
// and the source Unit recorded, so a non-USD account's figure carries
// its own currency label rather than a silently-wrong USD claim.
func parseUSDToMicroUSD(s string) (scanner.MicroUSD, error) {
	// Delegates to the canonical shared parser (slice 6 chunk 5,
	// v0.89.187). Kept as a named wrapper so the chunk-2 tests and
	// call sites stay stable.
	return scanner.ParseDecimalToMicroUSD(s)
}

// WithCostExplorerClient wires the Cost Explorer SDK client (or a test
// fake satisfying CostExplorerClient) into the Scanner. Returns the
// Scanner so the constructor chain composes, mirroring
// WithCloudWatchClient. Nil is accepted — QueryCost treats a nil
// client as not-wired (scanner.ErrCostNotImplemented).
func (s *Scanner) WithCostExplorerClient(client CostExplorerClient) *Scanner {
	s.costExplorerClient = client
	return s
}

// WithCostBudgetGovernor wires the per-account spend governor into the
// Scanner. Returns the Scanner so the constructor chain composes. Nil
// is accepted — QueryCost refuses to issue a charged call without a
// governor, so a nil governor disables cost queries (not an unbounded
// spend path).
func (s *Scanner) WithCostBudgetGovernor(g *scanner.CostBudgetGovernor) *Scanner {
	s.costGovernor = g
	return s
}

// --- Cost-correlation enrichment slice 6 chunk 3 (v0.89.185, #827
// Stream 224) — joins AWS SQS service cost onto DLQ-bearing queue
// snapshots so a poison-rate / DLQ recommendation can carry the
// operator-facing spend context ("Amazon SQS is costing ~$X/mo").
//
// SAFETY: like the entire metric substrate, this is plumbed but gated.
// enrichSQSCost is a NO-OP unless BOTH a Cost Explorer client and a
// budget governor are wired onto the Scanner (WithCostExplorerClient +
// WithCostBudgetGovernor) — and no production code wires them by
// default, so no charged Cost Explorer call fires in a scan until an
// operator explicitly opts in. The spend decision lives entirely at
// that wiring step.
//
// SPEND HYGIENE: at most ONE charged GetCostAndUsage call per scan,
// and only when at least one snapshot actually has a DLQ to correlate
// — a scan with no DLQ-bearing queues makes zero cost calls.

// AWSSQSCostServiceDimension is the Cost Explorer SERVICE dimension
// value for Amazon SQS. Cost is attributed at the service level (not
// per-queue — resource-level cost is a paid Cost Explorer opt-in the
// substrate deliberately avoids), so the figure is the account's total
// SQS spend, surfaced as context on queues that have an actionable DLQ.
const AWSSQSCostServiceDimension = "Amazon Simple Queue Service"

// AWSCostCorrelationWindowHours is the trailing window the cost figure
// covers (30 days), reported as a monthly figure.
const AWSCostCorrelationWindowHours = 30 * 24

// enrichSQSCost attaches the SQS service monthly cost to the Detail
// bag of every snapshot that has a DLQ (has_dlq == true) — the queues
// where "drain the DLQ to reduce spend" reasoning applies. Attaches
// three keys, clearly scoped:
//
//   - service_cost_monthly_micro_usd: the account's SQS spend over the
//     window, in micro-USD (integer).
//   - service_cost_currency: the source billing currency.
//   - service_cost_scope: always "service" — a HONEST label that this
//     is the SERVICE total, not a per-queue attribution.
//
// No-op (no charged call) when the cost client/governor are unwired,
// when no snapshot has a DLQ, or when the cost query is not Covered /
// over budget / errors. Keys are added only on a Covered reading, so
// the unwired path is byte-identical (cold-start parity).
//
// See docs/proposals/cost-correlation-substrate-slice6.md §6.
func (s *Scanner) enrichSQSCost(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot) {
	if s.costExplorerClient == nil || s.costGovernor == nil {
		return
	}
	// Spend hygiene: only issue the charged call if there is at least
	// one DLQ-bearing snapshot worth correlating cost to.
	anyDLQ := false
	for i := range snaps {
		if hasDLQ(snaps[i]) {
			anyDLQ = true
			break
		}
	}
	if !anyDLQ {
		return
	}

	res, err := s.QueryCost(
		ctx, AWSSQSCostServiceDimension, AWSSQSCostServiceDimension,
		time.Duration(AWSCostCorrelationWindowHours)*time.Hour,
	)
	if err != nil || !res.Covered {
		// Over budget / not measured / error → graceful skip, no keys.
		return
	}

	for i := range snaps {
		if !hasDLQ(snaps[i]) {
			continue
		}
		if snaps[i].Detail == nil {
			continue
		}
		snaps[i].Detail["service_cost_monthly_micro_usd"] = res.AmountMicroUSD
		snaps[i].Detail["service_cost_currency"] = res.Currency
		snaps[i].Detail["service_cost_scope"] = "service"
	}
}

// hasDLQ reports whether a snapshot's Detail bag carries has_dlq=true
// (set by applySQSDLQDetail). Defensive against a nil Detail or a
// non-bool value.
func hasDLQ(snap scanner.EventSourceInstanceSnapshot) bool {
	if snap.Detail == nil {
		return false
	}
	v, ok := snap.Detail["has_dlq"].(bool)
	return ok && v
}
