// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fixture builders -------------------------------------------------
//
// Two reusable builders keep the acceptance tests below readable —
// makeStateMachineListItem produces a single sfntypes.StateMachineListItem
// for the list-pass response; makeDescribeOutput produces the per-machine
// DescribeStateMachineOutput the scanner reads for the two detection
// axes.

func makeStateMachineListItem(name, arn string, smType sfntypes.StateMachineType) sfntypes.StateMachineListItem {
	return sfntypes.StateMachineListItem{
		Name:            awssdk.String(name),
		StateMachineArn: awssdk.String(arn),
		Type:            smType,
	}
}

func makeDescribeOutput(name, arn string, smType sfntypes.StateMachineType, traceEnabled bool, logLevel sfntypes.LogLevel) *sfn.DescribeStateMachineOutput {
	return &sfn.DescribeStateMachineOutput{
		Name:            awssdk.String(name),
		StateMachineArn: awssdk.String(arn),
		Type:            smType,
		TracingConfiguration: &sfntypes.TracingConfiguration{
			Enabled: traceEnabled,
		},
		LoggingConfiguration: &sfntypes.LoggingConfiguration{
			Level: logLevel,
		},
	}
}

// runStepFunctionsScan is the shared harness for the per-axis tests.
// Wires a fake SFN client into a fresh Scanner via the test factory
// builder and calls ScanStepFunctions against us-east-1. Returns the
// snapshots so the caller can assert per-axis outcomes without
// re-implementing the wiring boilerplate per test.
func runStepFunctionsScan(t *testing.T, fakeClient *fakeSFN) []scanner.OrchestrationInstanceSnapshot {
	t.Helper()
	factory := &fakeFactory{sfn: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanStepFunctions(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	return out
}

// TestStepFunctionsScanner_StandardWithXRayEnabled_HasTraceAxis —
// Standard workflow with TracingConfiguration.Enabled=true. Slice 1
// acceptance test 1: HasTraceAxis flips to true; WorkflowType is
// recorded as STANDARD.
func TestStepFunctionsScanner_StandardWithXRayEnabled_HasTraceAxis(t *testing.T) {
	const (
		name = "checkout"
		arn  = "arn:aws:states:us-east-1:123456789012:stateMachine:checkout"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{StateMachines: []sfntypes.StateMachineListItem{
				makeStateMachineListItem(name, arn, sfntypes.StateMachineTypeStandard),
			}},
		},
		describeByARN: map[string]*sfn.DescribeStateMachineOutput{
			arn: makeDescribeOutput(name, arn, sfntypes.StateMachineTypeStandard, true, sfntypes.LogLevelOff),
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	require.Len(t, out, 1)
	snap := out[0]
	assert.Equal(t, "aws", snap.Provider)
	assert.Equal(t, "stepfunc", snap.Surface)
	assert.Equal(t, "123456789012", snap.AccountID)
	assert.Equal(t, "us-east-1", snap.Region)
	assert.Equal(t, name, snap.ResourceName)
	assert.Equal(t, arn, snap.ResourceARN)
	assert.Equal(t, "STANDARD", snap.WorkflowType)
	assert.True(t, snap.HasTraceAxis, "trace axis must flip on TracingConfiguration.Enabled=true")
	assert.False(t, snap.HasLogAxis, "log axis must stay false when LoggingConfiguration.Level=OFF")
}

// TestStepFunctionsScanner_StandardWithoutXRay_NoTraceAxis — Standard
// workflow with TracingConfiguration.Enabled=false. Slice 1 acceptance
// test 2: HasTraceAxis stays false.
func TestStepFunctionsScanner_StandardWithoutXRay_NoTraceAxis(t *testing.T) {
	const (
		name = "orders"
		arn  = "arn:aws:states:us-east-1:123456789012:stateMachine:orders"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{StateMachines: []sfntypes.StateMachineListItem{
				makeStateMachineListItem(name, arn, sfntypes.StateMachineTypeStandard),
			}},
		},
		describeByARN: map[string]*sfn.DescribeStateMachineOutput{
			arn: makeDescribeOutput(name, arn, sfntypes.StateMachineTypeStandard, false, sfntypes.LogLevelOff),
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis, "trace axis must stay false when TracingConfiguration.Enabled=false")
	assert.False(t, out[0].HasLogAxis, "log axis must stay false when LoggingConfiguration.Level=OFF")
}

// TestStepFunctionsScanner_ExpressType_WorkflowTypeRecorded — EXPRESS
// workflow surfaces with WorkflowType="EXPRESS". The EXPRESS coverage
// caveat in design doc §12 documents that EXPRESS is treated
// identically to STANDARD at the detection axes; this test pins the
// type capture without asserting on the axes themselves.
func TestStepFunctionsScanner_ExpressType_WorkflowTypeRecorded(t *testing.T) {
	const (
		name = "express-bg"
		arn  = "arn:aws:states:us-east-1:123456789012:stateMachine:express-bg"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{StateMachines: []sfntypes.StateMachineListItem{
				makeStateMachineListItem(name, arn, sfntypes.StateMachineTypeExpress),
			}},
		},
		describeByARN: map[string]*sfn.DescribeStateMachineOutput{
			arn: makeDescribeOutput(name, arn, sfntypes.StateMachineTypeExpress, true, sfntypes.LogLevelError),
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "EXPRESS", out[0].WorkflowType, "EXPRESS surfaces verbatim")
	assert.True(t, out[0].HasTraceAxis)
	assert.True(t, out[0].HasLogAxis, "ERROR-level logging flips the log axis (any non-OFF level qualifies)")
	// The Detail bag carries the workflow_type pair for the per-cloud
	// Inventory tab's drilldown.
	assert.Equal(t, "EXPRESS", out[0].Detail["workflow_type"])
}

// TestStepFunctionsScanner_LoggingLevelNotOff_HasLogAxis — any
// LoggingConfiguration.Level other than OFF (and other than the
// empty-string sentinel) flips HasLogAxis. Slice 1 acceptance test 4.
// Drives the ALL case; the ERROR case is covered by the EXPRESS test
// above.
func TestStepFunctionsScanner_LoggingLevelNotOff_HasLogAxis(t *testing.T) {
	const (
		name = "audit"
		arn  = "arn:aws:states:us-east-1:123456789012:stateMachine:audit"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{StateMachines: []sfntypes.StateMachineListItem{
				makeStateMachineListItem(name, arn, sfntypes.StateMachineTypeStandard),
			}},
		},
		describeByARN: map[string]*sfn.DescribeStateMachineOutput{
			arn: makeDescribeOutput(name, arn, sfntypes.StateMachineTypeStandard, false, sfntypes.LogLevelAll),
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasLogAxis, "LogLevelAll flips the log axis")
	assert.False(t, out[0].HasTraceAxis, "trace axis remains independent of log axis")
}

// TestStepFunctionsScanner_LoggingLevelOff_NoLogAxis — LoggingConfiguration.Level=OFF
// leaves HasLogAxis false. Slice 1 acceptance test 5.
func TestStepFunctionsScanner_LoggingLevelOff_NoLogAxis(t *testing.T) {
	const (
		name = "silent"
		arn  = "arn:aws:states:us-east-1:123456789012:stateMachine:silent"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{StateMachines: []sfntypes.StateMachineListItem{
				makeStateMachineListItem(name, arn, sfntypes.StateMachineTypeStandard),
			}},
		},
		describeByARN: map[string]*sfn.DescribeStateMachineOutput{
			arn: makeDescribeOutput(name, arn, sfntypes.StateMachineTypeStandard, false, sfntypes.LogLevelOff),
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis, "LogLevelOff must leave HasLogAxis=false")
}

// TestStepFunctionsScanner_PaginationFollowsNextToken — two list pages
// surface three total machines. Slice 1 acceptance test 6: the scanner
// must follow NextToken until the response carries a nil / empty
// continuation token.
func TestStepFunctionsScanner_PaginationFollowsNextToken(t *testing.T) {
	const (
		arn1 = "arn:aws:states:us-east-1:123456789012:stateMachine:a"
		arn2 = "arn:aws:states:us-east-1:123456789012:stateMachine:b"
		arn3 = "arn:aws:states:us-east-1:123456789012:stateMachine:c"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{
				StateMachines: []sfntypes.StateMachineListItem{
					makeStateMachineListItem("a", arn1, sfntypes.StateMachineTypeStandard),
					makeStateMachineListItem("b", arn2, sfntypes.StateMachineTypeStandard),
				},
				NextToken: awssdk.String("page-2"),
			},
			{
				StateMachines: []sfntypes.StateMachineListItem{
					makeStateMachineListItem("c", arn3, sfntypes.StateMachineTypeExpress),
				},
				// No NextToken — terminal page.
			},
		},
		describeByARN: map[string]*sfn.DescribeStateMachineOutput{
			arn1: makeDescribeOutput("a", arn1, sfntypes.StateMachineTypeStandard, true, sfntypes.LogLevelAll),
			arn2: makeDescribeOutput("b", arn2, sfntypes.StateMachineTypeStandard, false, sfntypes.LogLevelOff),
			arn3: makeDescribeOutput("c", arn3, sfntypes.StateMachineTypeExpress, true, sfntypes.LogLevelError),
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	assert.Len(t, out, 3, "both list pages must surface — three machines total")

	arns := map[string]bool{}
	for _, snap := range out {
		arns[snap.ResourceARN] = true
	}
	assert.True(t, arns[arn1])
	assert.True(t, arns[arn2])
	assert.True(t, arns[arn3])
}

// TestStepFunctionsScanner_EmptyResponseReturnsEmptySlice — zero state
// machines surface as an empty result without error. The slice 1
// acceptance test for the empty-account scenario.
func TestStepFunctionsScanner_EmptyResponseReturnsEmptySlice(t *testing.T) {
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{}, // empty StateMachines slice, nil NextToken — terminal
		},
	}
	out := runStepFunctionsScan(t, fakeClient)
	assert.Len(t, out, 0)
}

// TestStepFunctionsScanner_DescribeFailureContinuesWithRemaining — the
// slice 1 acceptance test 8 contract: a single failing
// DescribeStateMachine call must not abort the whole scan. The fake's
// describeErr flips every describe call to fail; the scan should
// return an empty slice (since every per-machine describe failed) and
// MUST NOT propagate the error.
//
// This is the all-failures variant; a per-machine-selective injection
// is left to slice 2 if a finer signal proves useful. The key contract
// — the scanner does not panic or return an error when describe fails
// — is exercised here.
func TestStepFunctionsScanner_DescribeFailureContinuesWithRemaining(t *testing.T) {
	const (
		arn1 = "arn:aws:states:us-east-1:123456789012:stateMachine:a"
		arn2 = "arn:aws:states:us-east-1:123456789012:stateMachine:b"
	)
	fakeClient := &fakeSFN{
		listPages: []*sfn.ListStateMachinesOutput{
			{StateMachines: []sfntypes.StateMachineListItem{
				makeStateMachineListItem("a", arn1, sfntypes.StateMachineTypeStandard),
				makeStateMachineListItem("b", arn2, sfntypes.StateMachineTypeStandard),
			}},
		},
		describeErr: errors.New("simulated describe failure"),
	}
	factory := &fakeFactory{sfn: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanStepFunctions(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "describe failures must not propagate as scan error")
	assert.Len(t, out, 0, "every describe failed; the slice is empty rather than partial")
	// Defense-in-depth: confirm the describe was actually attempted
	// for each machine (the retryWithBackoff helper retries on
	// transient errors so the per-machine call count is bounded but
	// non-zero).
	assert.GreaterOrEqual(t, fakeClient.describeCalls, 2, "describe must have been attempted for each machine")
}

// TestOrchestrationInstanceSnapshot_IsInstrumented — slice 1 acceptance
// test for the OR-rule predicate on the snapshot. Either axis presence
// flips the predicate; both axes false stays uninstrumented.
func TestOrchestrationInstanceSnapshot_IsInstrumented(t *testing.T) {
	cases := []struct {
		name  string
		trace bool
		log   bool
		want  bool
	}{
		{"both off", false, false, false},
		{"trace only", true, false, true},
		{"log only", false, true, true},
		{"both on", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := scanner.OrchestrationInstanceSnapshot{
				HasTraceAxis: tc.trace,
				HasLogAxis:   tc.log,
			}
			assert.Equal(t, tc.want, snap.IsInstrumented())
		})
	}
}
