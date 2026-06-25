// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Cost-correlation opt-in wiring slice 6 chunk 6 (v0.89.188, #830
// Stream 227) — tests for EnableCostCorrelation, the production switch
// that flips the cost path live.

// costFakeFactory embeds the test fakeFactory and adds the
// CostExplorer builder method so it satisfies costExplorerBuilder.
type costFakeFactory struct {
	fakeFactory
	ce CostExplorerClient
}

func (f *costFakeFactory) CostExplorer() CostExplorerClient { return f.ce }

// --- success: wires real-shaped client + governor ----------------

func TestEnableCostCorrelation_WiresClientAndGovernor(t *testing.T) {
	s := costTestScanner()
	s.factory = &costFakeFactory{ce: &ceFake{}}

	err := s.EnableCostCorrelation(context.Background(), 5.0)
	assert.NoError(t, err)
	assert.NotNil(t, s.costExplorerClient, "cost client wired")
	assert.NotNil(t, s.costGovernor, "governor wired")
	if s.costGovernor != nil {
		assert.Equal(t, int64(5_000_000), s.costGovernor.Ceiling(), "$5 budget → $5.00 ceiling in micro-USD")
	}
}

// --- error: factory can't build a Cost Explorer client -----------

func TestEnableCostCorrelation_FactoryWithoutBuilderErrors(t *testing.T) {
	s := costTestScanner()
	s.factory = &fakeFactory{} // no CostExplorer method

	err := s.EnableCostCorrelation(context.Background(), 1.0)
	assert.Error(t, err, "non-Cost-Explorer factory must fail loudly, not silently make charged calls")
	assert.Nil(t, s.costExplorerClient, "no client wired on error")
	assert.Nil(t, s.costGovernor, "no governor wired on error")
}

// --- default-off invariant: a scanner that never calls Enable stays
// cost-dormant (QueryCost refuses, no charged calls). This is the
// safe default the opt-in protects.

func TestScanner_CostDormantByDefault(t *testing.T) {
	s := costTestScanner()
	assert.Nil(t, s.costExplorerClient, "no cost client until EnableCostCorrelation is called")
	assert.Nil(t, s.costGovernor, "no governor until opt-in")
}
