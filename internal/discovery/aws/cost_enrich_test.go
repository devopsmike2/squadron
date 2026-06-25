// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Cost-correlation enrichment slice 6 chunk 3 (v0.89.185, #827 Stream
// 224) — acceptance tests for enrichSQSCost.

func dlqSnap(name string) scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceName: name,
		Surface:      SQSSurface,
		Detail:       map[string]any{"has_dlq": true},
	}
}

func noDLQSnap(name string) scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceName: name,
		Surface:      SQSSurface,
		Detail:       map[string]any{"has_dlq": false},
	}
}

func wiredCostScanner(ce *ceFake, gov *scanner.CostBudgetGovernor) *Scanner {
	return costTestScanner().WithCostExplorerClient(ce).WithCostBudgetGovernor(gov)
}

// --- attaches service cost to DLQ-bearing snapshots when wired ---

func TestEnrichSQSCost_AttachesToDLQSnapshots(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("42.50")}
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := wiredCostScanner(ce, gov)

	snaps := []scanner.EventSourceInstanceSnapshot{dlqSnap("orders"), noDLQSnap("plain")}
	s.enrichSQSCost(context.Background(), snaps)

	// DLQ-bearing snapshot gets the cost keys, clearly scoped service-level.
	assert.Equal(t, scanner.MicroUSD(42_500_000), snaps[0].Detail["service_cost_monthly_micro_usd"])
	assert.Equal(t, "USD", snaps[0].Detail["service_cost_currency"])
	assert.Equal(t, "service", snaps[0].Detail["service_cost_scope"])

	// Non-DLQ snapshot untouched.
	_, has := snaps[1].Detail["service_cost_monthly_micro_usd"]
	assert.False(t, has, "non-DLQ snapshot does not get cost keys")

	// Exactly one charged call for the whole scan.
	assert.Equal(t, 1, ce.calls)
	assert.Equal(t, AWSCostExplorerGetCostAndUsageMicroUSD, gov.Spent())
}

// --- no-op when unwired (no client / no governor) ---------------

func TestEnrichSQSCost_NoOpWhenUnwired(t *testing.T) {
	// No client, no governor.
	s := costTestScanner()
	snaps := []scanner.EventSourceInstanceSnapshot{dlqSnap("orders")}
	s.enrichSQSCost(context.Background(), snaps)
	_, has := snaps[0].Detail["service_cost_monthly_micro_usd"]
	assert.False(t, has, "unwired cost path is a no-op (no keys, no spend)")
}

func TestEnrichSQSCost_NoOpWhenGovernorMissing(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("42.50")}
	s := costTestScanner().WithCostExplorerClient(ce) // governor nil
	snaps := []scanner.EventSourceInstanceSnapshot{dlqSnap("orders")}
	s.enrichSQSCost(context.Background(), snaps)
	_, has := snaps[0].Detail["service_cost_monthly_micro_usd"]
	assert.False(t, has)
	assert.Equal(t, 0, ce.calls, "no charged call without a governor")
}

// --- spend hygiene: no DLQ-bearing snapshot → zero charged calls -

func TestEnrichSQSCost_NoDLQNoChargedCall(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("42.50")}
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := wiredCostScanner(ce, gov)

	snaps := []scanner.EventSourceInstanceSnapshot{noDLQSnap("a"), noDLQSnap("b")}
	s.enrichSQSCost(context.Background(), snaps)

	assert.Equal(t, 0, ce.calls, "no DLQ to correlate → no charged Cost Explorer call (spend hygiene)")
	assert.Equal(t, scanner.MicroUSD(0), gov.Spent())
}

// --- budget exhausted → graceful skip (no keys) -----------------

func TestEnrichSQSCost_BudgetExhaustedNoKeys(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("42.50")}
	gov := scanner.NewCostBudgetGovernor(AWSCostExplorerGetCostAndUsageMicroUSD-1, time.Hour) // below one call
	s := wiredCostScanner(ce, gov)

	snaps := []scanner.EventSourceInstanceSnapshot{dlqSnap("orders")}
	s.enrichSQSCost(context.Background(), snaps)

	_, has := snaps[0].Detail["service_cost_monthly_micro_usd"]
	assert.False(t, has, "over-budget → graceful skip, no cost keys")
	assert.Equal(t, 0, ce.calls, "budget refused the call before it was issued")
}

// --- not-Covered (no billing rows) → no keys --------------------

func TestEnrichSQSCost_NotCoveredNoKeys(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput()} // empty results
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := wiredCostScanner(ce, gov)

	snaps := []scanner.EventSourceInstanceSnapshot{dlqSnap("orders")}
	s.enrichSQSCost(context.Background(), snaps)

	_, has := snaps[0].Detail["service_cost_monthly_micro_usd"]
	assert.False(t, has, "not-measured cost → no keys, not a false $0")
}
