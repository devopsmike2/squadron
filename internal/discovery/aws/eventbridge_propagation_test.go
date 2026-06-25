// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Slice 2 chunk 1 of the Event source tier arc (v0.89.105, #741 Stream
// 139). The tests in this file pin the per-rule propagation detection
// logic — rulePreservesTracePropagation — plus the bus-level
// aggregation: a single broken target on a single rule flips the
// bus axis to false; every-rule-preserves keeps it true; an empty bus
// stays vacuously true. The acceptance tests 1-6 + 15 from
// docs/proposals/event-source-tier-slice2.md §11 land here (15 lives
// in discovery_trace_coverage_propagation_test.go).
//
// Why this file is separate from eventbridge_test.go: the slice-1
// tests pinned the log-axis fallback; the slice-2 tests pin the
// per-target propagation surface. Keeping them in two files makes the
// pre-slice-2 / post-slice-2 diff obvious to reviewers and lets the
// chunk-1 commit land without rewriting the slice-1 fixture helpers.

// --- Fixture builders for InputPath / InputTransformer ----------------

// makeTargetWithInputPath returns a Target with the supplied InputPath
// (raw string, no leading-dollar handling). Pass "" to leave InputPath
// nil — the rulePreservesTracePropagation no-config case.
func makeTargetWithInputPath(id, arn, inputPath string) ebtypes.Target {
	t := ebtypes.Target{
		Id:  awssdk.String(id),
		Arn: awssdk.String(arn),
	}
	if inputPath != "" {
		t.InputPath = awssdk.String(inputPath)
	}
	return t
}

// makeTargetWithInputTransformer returns a Target with an
// InputTransformer whose InputTemplate is the supplied string. The
// InputPathsMap field is not loaded — the slice-2 chunk-1 detection
// rule only inspects the template string.
func makeTargetWithInputTransformer(id, arn, template string) ebtypes.Target {
	return ebtypes.Target{
		Id:  awssdk.String(id),
		Arn: awssdk.String(arn),
		InputTransformer: &ebtypes.InputTransformer{
			InputTemplate: awssdk.String(template),
		},
	}
}

// --- Per-target detection helper tests ---------------------------------

// TestRulePreservesTracePropagation_NoInputPathNoTransformer_Preserved
// — design doc §3.1 case 1, acceptance test 1. A target with neither
// an InputPath nor an InputTransformer carries the full event through
// to the destination, including the X-Ray trace header in the
// top-level event metadata. PROPAGATION PRESERVED.
func TestRulePreservesTracePropagation_NoInputPathNoTransformer_Preserved(t *testing.T) {
	preserved, note := rulePreservesTracePropagation("plain-rule", nil, nil)
	assert.True(t, preserved, "no InputPath / no InputTransformer must preserve")
	assert.Empty(t, note, "preserved targets carry no per-issue note")
}

// TestRulePreservesTracePropagation_InputPathDollar_Preserved — design
// doc §3.1 case 2, acceptance test 2. InputPath="$" is the AWS
// documented sentinel for "the whole event" — equivalent to no
// InputPath at all. PROPAGATION PRESERVED.
func TestRulePreservesTracePropagation_InputPathDollar_Preserved(t *testing.T) {
	path := "$"
	preserved, note := rulePreservesTracePropagation("dollar-rule", &path, nil)
	assert.True(t, preserved, `InputPath "$" must preserve (full event)`)
	assert.Empty(t, note)
}

// TestRulePreservesTracePropagation_InputPathDotDetail_Broken — design
// doc §3.1 case 3, acceptance test 3. InputPath="$.detail" projects
// only the event's detail object, dropping the top-level X-Amzn-Trace-Id
// header that EventBridge stamps into the event metadata. PROPAGATION
// BROKEN. The note names the rule + the offending InputPath so the
// proposer's reasoning text (chunk 5) can quote it verbatim.
func TestRulePreservesTracePropagation_InputPathDotDetail_Broken(t *testing.T) {
	path := "$.detail"
	preserved, note := rulePreservesTracePropagation("detail-rule", &path, nil)
	assert.False(t, preserved, `InputPath "$.detail" strips the top-level trace header`)
	require.NotEmpty(t, note)
	assert.Contains(t, note, "detail-rule")
	assert.Contains(t, note, "$.detail")
}

// TestRulePreservesTracePropagation_InputPathOther_Broken — defensive
// case: any InputPath value other than "$" strips the trace header.
// Pins the broader detection rule documented in design doc §3.1 case 3
// ("…or similar narrow path") beyond the canonical $.detail example.
func TestRulePreservesTracePropagation_InputPathOther_Broken(t *testing.T) {
	path := "$.detail.userIdentity"
	preserved, note := rulePreservesTracePropagation("narrow-rule", &path, nil)
	assert.False(t, preserved)
	assert.Contains(t, note, "$.detail.userIdentity")
}

// TestRulePreservesTracePropagation_InputTransformerWithXRayHeader_Preserved
// — design doc §3.1 case 4 happy path (X-Ray flavor), acceptance test
// 4. An InputTransformer template that contains the literal
// "x-amzn-trace-id" string preserves trace context — the operator
// explicitly forwarded the header through the transformer.
func TestRulePreservesTracePropagation_InputTransformerWithXRayHeader_Preserved(t *testing.T) {
	tmpl := `{"trace":"<trace>", "x-amzn-trace-id":"<tid>"}`
	xform := &ebtypes.InputTransformer{InputTemplate: awssdk.String(tmpl)}
	preserved, note := rulePreservesTracePropagation("xray-rule", nil, xform)
	assert.True(t, preserved, "template containing x-amzn-trace-id preserves propagation")
	assert.Empty(t, note)
}

// TestRulePreservesTracePropagation_InputTransformerWithXRayHeader_CaseInsensitive
// — pins the case-insensitive substring match. AWS canonical writes
// "X-Amzn-Trace-Id" with mixed case; the template authors in the wild
// sometimes use all-lower or all-upper. The detection rule normalizes.
func TestRulePreservesTracePropagation_InputTransformerWithXRayHeader_CaseInsensitive(t *testing.T) {
	tmpl := `{"X-Amzn-Trace-Id": "<tid>"}`
	xform := &ebtypes.InputTransformer{InputTemplate: awssdk.String(tmpl)}
	preserved, _ := rulePreservesTracePropagation("xray-mixed-case", nil, xform)
	assert.True(t, preserved, "X-Amzn-Trace-Id must match case-insensitively")
}

// TestRulePreservesTracePropagation_InputTransformerWithTraceparentHeader_Preserved
// — variant of test 4 using the W3C traceparent header instead of the
// AWS X-Ray header. Some operators on OTel-native flows prefer the W3C
// shape; the detection rule covers both.
func TestRulePreservesTracePropagation_InputTransformerWithTraceparentHeader_Preserved(t *testing.T) {
	tmpl := `{"traceparent": "<tp>", "tenant_id": "<tenant>"}`
	xform := &ebtypes.InputTransformer{InputTemplate: awssdk.String(tmpl)}
	preserved, note := rulePreservesTracePropagation("otel-rule", nil, xform)
	assert.True(t, preserved, "template containing traceparent preserves propagation")
	assert.Empty(t, note)
}

// TestRulePreservesTracePropagation_InputTransformerOmitsHeader_Broken
// — design doc §3.1 case 4 broken path, acceptance test 5. An
// InputTransformer template that mentions neither the X-Ray nor the
// W3C trace header strips trace propagation. The note names the rule
// so the proposer's chunk-5 reasoning can quote it.
func TestRulePreservesTracePropagation_InputTransformerOmitsHeader_Broken(t *testing.T) {
	tmpl := `{"order_id": "<oid>", "amount": "<amt>"}`
	xform := &ebtypes.InputTransformer{InputTemplate: awssdk.String(tmpl)}
	preserved, note := rulePreservesTracePropagation("orders-rule", nil, xform)
	assert.False(t, preserved, "template without trace header breaks propagation")
	require.NotEmpty(t, note)
	assert.Contains(t, note, "orders-rule")
	assert.Contains(t, note, "InputTransformer")
}

// --- Per-bus aggregation tests -----------------------------------------

// runEventBridgePropagationScan is a thin wrapper around
// runEventBridgeScan (defined in eventbridge_test.go) — slice-2 tests
// reuse the slice-1 harness. Kept as a separate name for clarity at the
// test call site that "this assertion is about propagation, not the
// log axis."
func runEventBridgePropagationScan(t *testing.T, f *fakeEventBridge) []map[string]any {
	t.Helper()
	out := runEventBridgeScan(t, f)
	rows := make([]map[string]any, 0, len(out))
	for _, snap := range out {
		rows = append(rows, map[string]any{
			"name":         snap.ResourceName,
			"has_prop":     snap.HasPropagationConfig,
			"notes":        snap.PropagationNotes,
			"rule_count":   snap.Detail["rule_count"],
			"has_log_axis": snap.HasLogAxis,
			"has_trace_ax": snap.HasTraceAxis,
		})
	}
	return rows
}

// TestEventBridgeScanner_BusWithAllRulesPreserving_HasPropagationConfig
// — every rule's targets carry no InputPath / no InputTransformer; the
// bus axis stays true.
func TestEventBridgeScanner_BusWithAllRulesPreserving_HasPropagationConfig(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "preserve-a"
		ruleB   = "preserve-b"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
				makeRule(ruleB, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleB),
			}}},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{makeNonLogTarget("t-a")}},
			ruleB: {Targets: []ebtypes.Target{makeNonLogTarget("t-b")}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasPropagationConfig, "all targets preserve → bus axis true")
	assert.Empty(t, out[0].PropagationNotes, "no broken targets → no notes")
}

// TestEventBridgeScanner_BusWithOneBrokenRule_NoPropagationConfig —
// acceptance test 6. A single broken target on a single rule flips the
// whole bus axis to false; the note names the offending rule. This pins
// the worst-case AND semantics — propagation is preserved on the bus
// iff EVERY target preserves.
func TestEventBridgeScanner_BusWithOneBrokenRule_NoPropagationConfig(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "preserve-a"
		ruleB   = "broken-detail"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{
				makeRule(ruleA, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleA),
				makeRule(ruleB, "arn:aws:events:us-east-1:123456789012:rule/default/"+ruleB),
			}}},
		},
		targetsByRule: map[string]*eventbridge.ListTargetsByRuleOutput{
			ruleA: {Targets: []ebtypes.Target{makeNonLogTarget("t-a")}},
			ruleB: {Targets: []ebtypes.Target{
				makeTargetWithInputPath("t-b",
					"arn:aws:lambda:us-east-1:123456789012:function:orders",
					"$.detail"),
			}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasPropagationConfig, "one broken rule fails the bus axis")
	require.Len(t, out[0].PropagationNotes, 1)
	assert.Contains(t, out[0].PropagationNotes[0], ruleB)
	assert.Contains(t, out[0].PropagationNotes[0], "$.detail")
}

// TestEventBridgeScanner_BusWithMixedRules_NotesAccumulate — each broken
// rule contributes a note. Two broken rules → two notes; the proposer's
// chunk-5 reasoning text walks the full list. The order in which notes
// surface follows the rule iteration order from ListRules.
func TestEventBridgeScanner_BusWithMixedRules_NotesAccumulate(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "preserve-a"
		ruleB   = "broken-input-path"
		ruleC   = "broken-transformer"
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
			ruleB: {Targets: []ebtypes.Target{
				makeTargetWithInputPath("t-b",
					"arn:aws:lambda:us-east-1:123456789012:function:b",
					"$.detail"),
			}},
			ruleC: {Targets: []ebtypes.Target{
				makeTargetWithInputTransformer("t-c",
					"arn:aws:lambda:us-east-1:123456789012:function:c",
					`{"order":"<o>"}`),
			}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasPropagationConfig)
	require.Len(t, out[0].PropagationNotes, 2, "two broken rules → two notes")
	// Look up by content so order is not assertion-fragile.
	noteSet := map[string]bool{}
	for _, n := range out[0].PropagationNotes {
		noteSet[n] = true
	}
	foundB := false
	foundC := false
	for n := range noteSet {
		if containsAll(n, ruleB, "InputPath") {
			foundB = true
		}
		if containsAll(n, ruleC, "InputTransformer") {
			foundC = true
		}
	}
	assert.True(t, foundB, "broken-input-path note must mention rule + InputPath")
	assert.True(t, foundC, "broken-transformer note must mention rule + InputTransformer")
}

// TestEventBridgeScanner_BusWithNoRules_PropagationConfigDefaultsTrue —
// vacuous truth: a bus with no rules has no targets to break
// propagation, so the axis stays true.
func TestEventBridgeScanner_BusWithNoRules_PropagationConfigDefaultsTrue(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
	)
	fakeClient := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{makeEventBus(busName, busARN)}},
		},
		listRulesPagesByBus: map[string][]*eventbridge.ListRulesOutput{
			busName: {{Rules: []ebtypes.Rule{}}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasPropagationConfig, "no rules → vacuously preserved")
	assert.Empty(t, out[0].PropagationNotes)
	assert.Equal(t, 0, out[0].Detail["rule_count"])
}

// TestEventBridgeScanner_RuleWithMultipleTargets_OneBrokenFailsRule —
// per-target inspection composes correctly with the per-rule
// aggregation: a single rule with one preserved target and one broken
// target still emits the broken note and flips the bus axis. The
// preserved target on the same rule does NOT cancel the broken one —
// propagation is a worst-case AND across every target. The rule_count
// stays at 1 because a rule is one row even with multiple targets.
func TestEventBridgeScanner_RuleWithMultipleTargets_OneBrokenFailsRule(t *testing.T) {
	const (
		busName = "default"
		busARN  = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		ruleA   = "mixed-targets"
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
			ruleA: {Targets: []ebtypes.Target{
				// Preserved target: no input config.
				makeNonLogTarget("t-preserve"),
				// Broken target: $.detail strips the trace header.
				makeTargetWithInputPath("t-broken",
					"arn:aws:lambda:us-east-1:123456789012:function:b",
					"$.detail"),
			}},
		},
	}
	out := runEventBridgeScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasPropagationConfig)
	require.Len(t, out[0].PropagationNotes, 1)
	assert.Contains(t, out[0].PropagationNotes[0], ruleA)
	assert.Equal(t, 1, out[0].Detail["rule_count"], "two targets on one rule still count as one rule")
}

// containsAll returns true when s contains every supplied substring.
// Test-local helper; avoids pulling in a wider matcher dependency.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !containsSubstr(s, sub) {
			return false
		}
	}
	return true
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Keep runEventBridgePropagationScan referenced so unused-helper
// detection stays quiet — it's available for future slice 2 tests
// that need a structured shape across multiple buses.
var _ = runEventBridgePropagationScan
