// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fixture builders -------------------------------------------------
//
// Three reusable builders keep the per-axis tests readable —
// makeEventBus produces a single ebtypes.EventBus for the list-pass
// response; makeRule produces an ebtypes.Rule for the per-bus rules
// list; makeLogTarget / makeNonLogTarget produce ebtypes.Target values
// the per-rule list-targets call returns.

func makeEventBus(name, arn string) ebtypes.EventBus {
	return ebtypes.EventBus{
		Name: awssdk.String(name),
		Arn:  awssdk.String(arn),
	}
}

func makeRule(name, arn string) ebtypes.Rule {
	return ebtypes.Rule{
		Name: awssdk.String(name),
		Arn:  awssdk.String(arn),
	}
}

func makeLogTarget(id string) ebtypes.Target {
	return ebtypes.Target{
		Id:  awssdk.String(id),
		Arn: awssdk.String("arn:aws:logs:us-east-1:123456789012:log-group:/aws/events/" + id),
	}
}

func makeNonLogTarget(id string) ebtypes.Target {
	return ebtypes.Target{
		Id:  awssdk.String(id),
		Arn: awssdk.String("arn:aws:lambda:us-east-1:123456789012:function:" + id),
	}
}

// runEventBridgeScan is the shared harness for the per-axis tests.
// Wires a fake EventBridge client into a fresh Scanner via the test
// factory builder and calls ScanEventBridge against us-east-1. Returns
// the snapshots so the caller can assert per-axis outcomes without
// re-implementing the wiring boilerplate per test.
func runEventBridgeScan(t *testing.T, fakeClient *fakeEventBridge) []scanner.EventSourceInstanceSnapshot {
	t.Helper()
	factory := &fakeFactory{eventbridge: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanEventBridge(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	return out
}

// TestEventBridgeScanner_BusWithLogTargetRule_HasLogAxis — slice 1
// acceptance test 3: a bus with a rule pointing at a CloudWatch Logs
// target flips HasLogAxis. The chunk-1 fallback (log-target proxy for
// both axes) also flips HasTraceAxis to true.
func TestEventBridgeScanner_BusWithLogTargetRule_HasLogAxis(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "log-everything"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
			}}},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{makeLogTarget("t1")}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	snap := out[0]
	assert.Equal(t, "aws", snap.Provider)
	assert.Equal(t, "eventbridge", snap.Surface)
	assert.Equal(t, "123456789012", snap.AccountID)
	assert.Equal(t, "us-east-1", snap.Region)
	assert.Equal(t, busName, snap.ResourceName)
	assert.Equal(t, busARN, snap.ResourceARN)
	assert.Equal(t, "bus", snap.SourceType)
	assert.True(t, snap.HasLogAxis, "log axis must flip on a CloudWatch Logs target ARN match")
	// Slice 1 chunk 1 fallback: a bus with a log target is the proxy for
	// trace observability readiness since CloudWatch Logs can carry the
	// X-Ray trace header.
	assert.True(t, snap.HasTraceAxis, "trace axis follows the log-target proxy in chunk 1")
	assert.Equal(t, 1, snap.Detail["rule_count"])
}

// TestEventBridgeScanner_BusWithoutLogTargetRule_NoLogAxis — a bus with
// only non-log targets (Lambda, etc.) leaves HasLogAxis false. The
// chunk-1 fallback means HasTraceAxis stays false too.
func TestEventBridgeScanner_BusWithoutLogTargetRule_NoLogAxis(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "invoke-lambda"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
			}}},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{makeNonLogTarget("t1")}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis, "log axis stays false when no target ARN matches the CloudWatch Logs prefix")
	assert.False(t, out[0].HasTraceAxis, "trace axis stays false in chunk 1 when log axis is false")
	assert.Equal(t, 1, out[0].Detail["rule_count"])
}

// TestEventBridgeScanner_BusWithRuleButNoTargets_NoLogAxis — a bus with
// rules that have an empty targets list. The rule_count still surfaces
// but neither axis flips.
func TestEventBridgeScanner_BusWithRuleButNoTargets_NoLogAxis(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "orphan-rule"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
			}}},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis)
	assert.False(t, out[0].HasTraceAxis)
	assert.Equal(t, 1, out[0].Detail["rule_count"])
}

// TestEventBridgeScanner_BusWithMultipleRules_AnyLogTargetSatisfies —
// any rule with a log target satisfies the axis; the detection
// short-circuits at the first hit without panic.
func TestEventBridgeScanner_BusWithMultipleRules_AnyLogTargetSatisfies(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "rule-a"
		ruleB   = "rule-b-with-log"
		ruleC   = "rule-c"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
				makeRule(ruleB, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleB),
				makeRule(ruleC, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleC),
			}}},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{makeNonLogTarget("t-a")}},
			ruleB: {Targets: []ebtypes.Target{makeNonLogTarget("t-b1"), makeLogTarget("t-b2")}},
			ruleC: {Targets: []ebtypes.Target{makeNonLogTarget("t-c")}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasLogAxis, "any rule with a log target flips the axis")
	assert.True(t, out[0].HasTraceAxis, "chunk-1 fallback proxies the log target to the trace axis")
	assert.Equal(t, 3, out[0].Detail["rule_count"])
}

// TestEventBridgeScanner_PaginationFollowsNextToken_ListEventBuses —
// two list-buses pages surface three total buses. The scanner must
// follow NextToken until the response carries a nil / empty token.
func TestEventBridgeScanner_PaginationFollowsNextToken_ListEventBuses(t *testing.T) {
	const (
		arn1 = "arn:aws:events:us-east-1:123456789012:event-bus/a"
		arn2 = "arn:aws:events:us-east-1:123456789012:event-bus/b"
		arn3 = "arn:aws:events:us-east-1:123456789012:event-bus/c"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{
				EventBuses: []ebtypes.EventBus{
					makeEventBus("a", arn1),
					makeEventBus("b", arn2),
				},
				NextToken: awssdk.String("page-2"),
			},
			{
				EventBuses: []ebtypes.EventBus{
					makeEventBus("c", arn3),
				},
			},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	assert.Len(t, out, 3, "both list pages must surface — three buses total")
	arns := map[string]bool{}
	for _, snap := range out {
		arns[snap.ResourceARN] = true
	}
	assert.True(t, arns[arn1])
	assert.True(t, arns[arn2])
	assert.True(t, arns[arn3])
}

// TestEventBridgeScanner_PaginationFollowsNextToken_ListRules — the
// per-bus ListRules call follows pagination. Two pages of rules surface
// in the rule_count tally, and a log-target on the second page still
// flips the axis.
func TestEventBridgeScanner_PaginationFollowsNextToken_ListRules(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "rule-page1"
		ruleB   = "rule-page2-with-log"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {
				{
					Rules: []ebtypes.Rule{
						makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
					},
					NextToken: awssdk.String("rules-page-2"),
				},
				{
					Rules: []ebtypes.Rule{
						makeRule(ruleB, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleB),
					},
				},
			},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{makeNonLogTarget("t-a")}},
			ruleB: {Targets: []ebtypes.Target{makeLogTarget("t-b")}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, 2, out[0].Detail["rule_count"], "both rule pages contribute to the count")
	assert.True(t, out[0].HasLogAxis, "a log target on the second page flips the axis")
}

// TestEventBridgeScanner_EmptyResponseReturnsEmptySlice — zero event
// buses surface as an empty result without error. The slice 1
// acceptance test for the empty-account scenario.
func TestEventBridgeScanner_EmptyResponseReturnsEmptySlice(t *testing.T) {
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{}, // empty EventBuses slice, nil NextToken — terminal
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	assert.Len(t, out, 0)
}

// TestEventBridgeScanner_ListTargetsFailureContinuesWithRemaining — a
// single failing ListTargetsByRule call must not abort the whole scan.
// The bus row still surfaces with its universal columns; the axes
// default to false when the per-rule walk fails.
func TestEventBridgeScanner_ListTargetsFailureContinuesWithRemaining(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "rule-a"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
			}}},
		},
		listTargetsErr: errors.New("simulated ListTargetsByRule failure"),
	}
	factory := &fakeFactory{eventbridge: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanEventBridge(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "ListTargetsByRule failure must not propagate as scan error")
	require.Len(t, out, 1, "the bus row still surfaces; axes default to false")
	assert.False(t, out[0].HasLogAxis)
	assert.False(t, out[0].HasTraceAxis)
}

// TestEventSourceInstanceSnapshot_IsInstrumented — slice 1 acceptance
// test for the OR-rule predicate on the snapshot. Either axis presence
// flips the predicate; both axes false stays uninstrumented.
func TestEventSourceInstanceSnapshot_IsInstrumented(t *testing.T) {
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
			snap := scanner.EventSourceInstanceSnapshot{
				HasTraceAxis: tc.trace,
				HasLogAxis:   tc.log,
			}
			assert.Equal(t, tc.want, snap.IsInstrumented())
		})
	}
}

// TestScanEventSources_DelegatesToScanEventBridge — the canonical
// dispatcher just forwards to ScanEventBridge.
func TestScanEventSources_DelegatesToScanEventBridge(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
	}
	factory := &fakeFactory{eventbridge: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, busARN, out[0].ResourceARN)
}
