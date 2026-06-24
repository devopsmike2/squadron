// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// TestParseTiersOrDefault_AcceptsOrchestration — orchestration-tier
// slice 1 chunk 1 regression: "orchestration" passes the validator and
// lands on the normalized list.
func TestParseTiersOrDefault_AcceptsOrchestration(t *testing.T) {
	got := parseTiersOrDefault([]string{"orchestration"})
	if len(got) != 1 || got[0] != TierOrchestration {
		t.Fatalf("parseTiersOrDefault([orchestration]) = %v, want [orchestration]", got)
	}
}

// TestScanHandler_DefaultTierListIncludesOrchestration — DefaultScanTiers
// contains TierOrchestration so the implicit empty-request scan covers
// the orchestration surface alongside compute / database / kubernetes /
// serverless.
func TestScanHandler_DefaultTierListIncludesOrchestration(t *testing.T) {
	found := false
	for _, tier := range DefaultScanTiers {
		if tier == TierOrchestration {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DefaultScanTiers missing TierOrchestration: %v", DefaultScanTiers)
	}
}

// TestMarshalScanResult_IncludesOrchestrations — a Result with
// Orchestrations entries surfaces them on the wire as
// awsOrchestrationRow values with the universal columns + per-surface
// Detail bag. Mirrors the serverless variant.
func TestMarshalScanResult_IncludesOrchestrations(t *testing.T) {
	seen := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := &scanner.Result{
		ScanID:    "scan-1",
		Provider:  credstore.ProviderAWS,
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		Orchestrations: []scanner.OrchestrationInstanceSnapshot{
			{
				Provider:     "aws",
				Surface:      "stepfunc",
				AccountID:    "123456789012",
				Region:       "us-east-1",
				ResourceName: "checkout",
				ResourceARN:  "arn:aws:states:us-east-1:123456789012:stateMachine:checkout",
				WorkflowType: "STANDARD",
				HasTraceAxis: true,
				HasLogAxis:   true,
				LastSeenAt:   &seen,
				Detail:       map[string]any{"workflow_type": "STANDARD"},
			},
		},
		InstrumentedCount: 1,
	}

	out := marshalScanResult(r)
	if len(out.Orchestrations) != 1 {
		t.Fatalf("Orchestrations on wire = %d, want 1", len(out.Orchestrations))
	}
	row := out.Orchestrations[0]
	if row.Provider != "aws" || row.Surface != "stepfunc" {
		t.Errorf("provider/surface = %q/%q, want aws/stepfunc", row.Provider, row.Surface)
	}
	if row.WorkflowType != "STANDARD" {
		t.Errorf("workflow_type lost on wire: got %q", row.WorkflowType)
	}
	if !row.HasTraceAxis || !row.HasLogAxis {
		t.Errorf("axes lost on wire: trace=%v log=%v", row.HasTraceAxis, row.HasLogAxis)
	}
	if row.LastSeenAt == nil || !row.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt lost on wire: got %v", row.LastSeenAt)
	}
	if got, _ := row.Detail["workflow_type"].(string); got != "STANDARD" {
		t.Errorf("Detail[workflow_type] lost on wire: got %q", got)
	}
}

// TestInventoryHandler_ResponseIncludesOrchestrationsField — the JSON
// response carries the "orchestrations" key alongside the other
// per-tier wire fields. Empty Orchestrations surfaces as "[]" rather
// than "null", matching the non-null posture on every other category
// array (compute / databases / clusters / serverless / etc.).
func TestInventoryHandler_ResponseIncludesOrchestrationsField(t *testing.T) {
	r := &scanner.Result{
		ScanID:    "scan-1",
		Provider:  credstore.ProviderAWS,
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	}
	out := marshalScanResult(r)
	if out.Orchestrations == nil {
		t.Errorf("Orchestrations = nil, want []awsOrchestrationRow{}")
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe["orchestrations"]; !ok {
		t.Errorf("response missing 'orchestrations' field. keys=%v", keysOf(probe))
	}

	// The JSON field must contain "[]" (or equivalent) not "null" —
	// the per-cloud Inventory tab's empty-state branch is a single
	// `.length === 0` check.
	if !containsServerlessTokens(string(raw), `"orchestrations":[]`) {
		t.Errorf("orchestrations field not non-null array. raw=%s", string(raw))
	}
}

// TestOCIInventoryHandler_OrchestrationsFieldIsEmpty — slice 1 chunk 1
// of the orchestration-tier arc does not light up OCI orchestration
// coverage (OCI has no first-class orchestration surface in slice 1;
// design doc §5 explicitly defers to slice 2). The full per-OCI wire
// integration lands with chunk 4 of the arc, when the per-provider
// inventory handler grows the orchestration field across all four
// clouds. TODO: enable this once the OCI inventory handler exposes
// orchestrations end-to-end (chunk 4).
func TestOCIInventoryHandler_OrchestrationsFieldIsEmpty(t *testing.T) {
	t.Skip("OCI orchestration inventory wiring lands with chunk 4 of the orchestration-tier arc — slice 1 chunk 1 only ships the AWS surface")
}
