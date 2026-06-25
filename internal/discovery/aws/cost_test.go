// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Cost-correlation substrate slice 6 chunk 2 (v0.89.184, #826 Stream
// 223) — acceptance tests for the AWS Cost Explorer QueryCost body.

// ceFake is a single-method fake for CostExplorerClient.
type ceFake struct {
	calls          int
	respondWith    *costexplorer.GetCostAndUsageOutput
	respondErr     error
	receivedInputs []*costexplorer.GetCostAndUsageInput
}

func (f *ceFake) GetCostAndUsage(
	_ context.Context,
	in *costexplorer.GetCostAndUsageInput,
	_ ...func(*costexplorer.Options),
) (*costexplorer.GetCostAndUsageOutput, error) {
	f.calls++
	f.receivedInputs = append(f.receivedInputs, in)
	return f.respondWith, f.respondErr
}

func ceOutput(amounts ...string) *costexplorer.GetCostAndUsageOutput {
	rbt := make([]cetypes.ResultByTime, 0, len(amounts))
	for _, a := range amounts {
		rbt = append(rbt, cetypes.ResultByTime{
			Total: map[string]cetypes.MetricValue{
				awsCostMetricUnblended: {Amount: awssdk.String(a), Unit: awssdk.String("USD")},
			},
		})
	}
	return &costexplorer.GetCostAndUsageOutput{ResultsByTime: rbt}
}

func costTestScanner() *Scanner {
	return NewScannerForValidation(credstore.AWSCredentials{}, "123456789012")
}

// --- per-call cost const pin ------------------------------------

func TestAWSCostExplorerPerCallCost_Constant(t *testing.T) {
	assert.Equal(t, scanner.MicroUSDPerCent, AWSCostExplorerGetCostAndUsageMicroUSD, "Cost Explorer ~$0.01/call")
}

// --- guardrail: no charged call without client AND governor -----

func TestQueryCost_NilClientReturnsNotImplemented(t *testing.T) {
	s := costTestScanner().WithCostBudgetGovernor(scanner.NewCostBudgetGovernor(0, 0))
	// client still nil
	_, err := s.QueryCost(context.Background(), "sqs-dlq", "Amazon Simple Queue Service", 30*24*time.Hour)
	assert.ErrorIs(t, err, scanner.ErrCostNotImplemented)
}

func TestQueryCost_NilGovernorRefusesChargedCall(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("5.00")}
	s := costTestScanner().WithCostExplorerClient(ce)
	// governor nil → must refuse, MUST NOT call the charged API.
	_, err := s.QueryCost(context.Background(), "sqs-dlq", "Amazon Simple Queue Service", 30*24*time.Hour)
	assert.ErrorIs(t, err, scanner.ErrCostNotImplemented)
	assert.Equal(t, 0, ce.calls, "no charged Cost Explorer call without a governor")
}

// --- budget exhausted → graceful skip ---------------------------

func TestQueryCost_BudgetExhaustedGracefulSkip(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("5.00")}
	// Ceiling below one call's cost → first Authorize fails.
	gov := scanner.NewCostBudgetGovernor(AWSCostExplorerGetCostAndUsageMicroUSD-1, time.Hour)
	s := costTestScanner().WithCostExplorerClient(ce).WithCostBudgetGovernor(gov)

	res, err := s.QueryCost(context.Background(), "sqs-dlq", "Amazon Simple Queue Service", 30*24*time.Hour)
	assert.ErrorIs(t, err, scanner.ErrCostBudgetExceeded, "over-budget is a graceful skip sentinel")
	assert.False(t, res.Covered, "not measured when budget refuses the call")
	assert.Equal(t, 0, ce.calls, "no charged call issued when budget refuses")
}

// --- real cost: SERVICE filter + UnblendedCost sum → micro-USD --

func TestQueryCost_SumsUnblendedToMicroUSD(t *testing.T) {
	ce := &ceFake{respondWith: ceOutput("12.345678", "7.654322")} // sum 20.000000
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costTestScanner().WithCostExplorerClient(ce).WithCostBudgetGovernor(gov)

	res, err := s.QueryCost(context.Background(), "sqs-dlq", "Amazon Simple Queue Service", 30*24*time.Hour)
	assert.NoError(t, err)
	assert.True(t, res.Covered)
	assert.Equal(t, scanner.MicroUSD(20_000_000), res.AmountMicroUSD, "$12.345678 + $7.654322 = $20.000000")
	assert.Equal(t, "USD", res.Currency)
	assert.Equal(t, scanner.CostGranularityMonthly, res.Granularity)

	// Governor recorded exactly one call's spend.
	assert.Equal(t, AWSCostExplorerGetCostAndUsageMicroUSD, gov.Spent())

	// Filter asserts SERVICE dimension = the requested value.
	if assert.Len(t, ce.receivedInputs, 1) {
		in := ce.receivedInputs[0]
		assert.Equal(t, []string{awsCostMetricUnblended}, in.Metrics)
		if assert.NotNil(t, in.Filter) && assert.NotNil(t, in.Filter.Dimensions) {
			assert.Equal(t, cetypes.DimensionService, in.Filter.Dimensions.Key)
			assert.Equal(t, []string{"Amazon Simple Queue Service"}, in.Filter.Dimensions.Values)
		}
	}
}

func TestQueryCost_EmptyResultsNotCovered(t *testing.T) {
	ce := &ceFake{respondWith: &costexplorer.GetCostAndUsageOutput{ResultsByTime: nil}}
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costTestScanner().WithCostExplorerClient(ce).WithCostBudgetGovernor(gov)

	res, err := s.QueryCost(context.Background(), "sqs-dlq", "Amazon Simple Queue Service", 30*24*time.Hour)
	assert.NoError(t, err)
	assert.False(t, res.Covered, "no billing rows → not measured, not a false $0")
	assert.Equal(t, scanner.MicroUSD(0), res.AmountMicroUSD)
}

// --- parseUSDToMicroUSD edge cases (no float drift) -------------

func TestParseUSDToMicroUSD(t *testing.T) {
	cases := []struct {
		in   string
		want scanner.MicroUSD
		err  bool
	}{
		{"0", 0, false},
		{"1", 1_000_000, false},
		{"0.01", 10_000, false},
		{"12.345678", 12_345_678, false},
		{"12.3456789", 12_345_678, false}, // 7th decimal truncated
		{"100.5", 100_500_000, false},
		{".5", 500_000, false},
		{"-3.25", -3_250_000, false},
		{"  4.00 ", 4_000_000, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.2x", 0, true},
	}
	for _, c := range cases {
		got, err := parseUSDToMicroUSD(c.in)
		if c.err {
			assert.Error(t, err, "input %q should error", c.in)
			continue
		}
		assert.NoError(t, err, "input %q", c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
	}
}

// --- API error surfaces (governor already charged the attempt) --

func TestQueryCost_APIErrorSurfaces(t *testing.T) {
	ce := &ceFake{respondErr: errors.New("AccessDeniedException")}
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costTestScanner().WithCostExplorerClient(ce).WithCostBudgetGovernor(gov)

	_, err := s.QueryCost(context.Background(), "sqs-dlq", "Amazon Simple Queue Service", 30*24*time.Hour)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cost explorer get cost and usage")
}
