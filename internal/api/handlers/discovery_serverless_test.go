// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// TestParseTiersOrDefault_EmptyFallsBackToDefault — the event-source-
// tier slice 1 chunk 1 default tier list includes "serverless",
// "orchestration", and "event_source" alongside the historical compute
// / database / kubernetes entries. An empty request yields the full
// DefaultScanTiers slice. Six tiers as of v0.89.100.
func TestParseTiersOrDefault_EmptyFallsBackToDefault(t *testing.T) {
	got := parseTiersOrDefault(nil)
	if len(got) != 6 {
		t.Fatalf("default tiers length = %d, want 6", len(got))
	}
	wantSet := map[string]bool{
		TierCompute: true, TierDatabase: true,
		TierKubernetes: true, TierServerless: true,
		TierOrchestration: true, TierEventSource: true,
	}
	for _, tier := range got {
		if !wantSet[tier] {
			t.Errorf("unexpected tier in default: %q", tier)
		}
		delete(wantSet, tier)
	}
	if len(wantSet) != 0 {
		t.Errorf("default tier list missing entries: %v", wantSet)
	}
}

// TestParseTiersOrDefault_AcceptsServerless — slice 1 chunk 1
// regression: "serverless" passes the validator and lands on the
// normalized list.
func TestParseTiersOrDefault_AcceptsServerless(t *testing.T) {
	got := parseTiersOrDefault([]string{"serverless"})
	if len(got) != 1 || got[0] != TierServerless {
		t.Fatalf("parseTiersOrDefault([serverless]) = %v, want [serverless]", got)
	}
}

// TestParseTiersOrDefault_DropsUnknownTiers — an operator typo
// shouldn't 400 the scan endpoint. Unknown tiers are filtered out
// silently; the surviving recognized tiers carry through.
func TestParseTiersOrDefault_DropsUnknownTiers(t *testing.T) {
	got := parseTiersOrDefault([]string{"compute", "bogus", "serverless"})
	want := map[string]bool{TierCompute: true, TierServerless: true}
	if len(got) != 2 {
		t.Fatalf("expected 2 surviving tiers, got %v", got)
	}
	for _, tier := range got {
		if !want[tier] {
			t.Errorf("unexpected surviving tier: %q", tier)
		}
	}
}

// TestParseTiersOrDefault_AllUnknownFallsBackToDefault — every tier
// being unrecognized falls back to the default rather than scanning
// nothing. Six tiers as of v0.89.100 (event-source-tier slice 1 chunk 1).
func TestParseTiersOrDefault_AllUnknownFallsBackToDefault(t *testing.T) {
	got := parseTiersOrDefault([]string{"bogus", "alsobogus"})
	if len(got) != 6 {
		t.Errorf("expected 6 fallback tiers, got %d: %v", len(got), got)
	}
}

// TestParseTiersOrDefault_NormalizesCase — uppercase / mixed-case
// inputs are accepted (operators frequently hand-roll JSON with
// title-case enums).
func TestParseTiersOrDefault_NormalizesCase(t *testing.T) {
	got := parseTiersOrDefault([]string{"SERVERLESS", " Compute "})
	want := map[string]bool{TierCompute: true, TierServerless: true}
	if len(got) != 2 {
		t.Fatalf("got %d tiers, want 2: %v", len(got), got)
	}
	for _, tier := range got {
		if !want[tier] {
			t.Errorf("unexpected tier: %q", tier)
		}
	}
}

// TestMarshalScanResult_IncludesServerless — a Result with
// Serverless entries surfaces them on the wire as awsServerlessRow
// values with the universal columns + per-surface Detail bag.
func TestMarshalScanResult_IncludesServerless(t *testing.T) {
	seen := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := &scanner.Result{
		ScanID:    "scan-1",
		Provider:  credstore.ProviderAWS,
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		Serverless: []scanner.ServerlessInstanceSnapshot{
			{
				Provider:      "aws",
				Surface:       "lambda",
				AccountID:     "123456789012",
				Region:        "us-east-1",
				ResourceName:  "checkout",
				ResourceARN:   "arn:aws:lambda:us-east-1:123456789012:function:checkout",
				Runtime:       "python3.11",
				HasTraceAxis:  true,
				HasOTelDistro: true,
				LastSeenAt:    &seen,
				Detail:        map[string]any{"x_ray_mode": "Active", "layer_count": 2},
			},
		},
		InstrumentedCount: 1,
	}

	out := marshalScanResult(r)
	if len(out.Serverless) != 1 {
		t.Fatalf("Serverless on wire = %d, want 1", len(out.Serverless))
	}
	row := out.Serverless[0]
	if row.Provider != "aws" || row.Surface != "lambda" {
		t.Errorf("provider/surface = %q/%q, want aws/lambda", row.Provider, row.Surface)
	}
	if !row.HasTraceAxis || !row.HasOTelDistro {
		t.Errorf("axes lost on wire: trace=%v otel=%v", row.HasTraceAxis, row.HasOTelDistro)
	}
	if row.LastSeenAt == nil || !row.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt lost on wire: got %v", row.LastSeenAt)
	}
	if got, _ := row.Detail["x_ray_mode"].(string); got != "Active" {
		t.Errorf("Detail[x_ray_mode] lost on wire: got %q", got)
	}

	// JSON round-trip — the wire field must be "serverless".
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe["serverless"]; !ok {
		t.Errorf("response missing 'serverless' field. keys=%v", keysOf(probe))
	}
}

// TestMarshalScanResult_EmptyServerless_NonNullSlice — an empty
// Serverless slice surfaces on the wire as `[]`, not `null`. Matches
// the existing posture for compute / databases / clusters arrays.
func TestMarshalScanResult_EmptyServerless_NonNullSlice(t *testing.T) {
	r := &scanner.Result{
		ScanID:    "scan-1",
		Provider:  credstore.ProviderAWS,
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	}
	out := marshalScanResult(r)
	if out.Serverless == nil {
		t.Errorf("Serverless = nil, want []awsServerlessRow{}")
	}
	raw, _ := json.Marshal(out)
	// The JSON field must contain "[]" (or equivalent) not "null".
	if !containsServerlessTokens(string(raw), `"serverless":[]`) {
		t.Errorf("serverless field not non-null array. raw=%s", string(raw))
	}
}

// TestServerlessSnapshot_IsInstrumented_ORRule — the slice 1 OR
// rule: either axis on counts as instrumented. Both-off counts as
// uninstrumented.
func TestServerlessSnapshot_IsInstrumented_ORRule(t *testing.T) {
	cases := []struct {
		name      string
		hasTrace  bool
		hasOTel   bool
		wantTrue  bool
	}{
		{"both off", false, false, false},
		{"trace only", true, false, true},
		{"otel only", false, true, true},
		{"both on", true, true, true},
	}
	for _, tc := range cases {
		snap := scanner.ServerlessInstanceSnapshot{
			HasTraceAxis:  tc.hasTrace,
			HasOTelDistro: tc.hasOTel,
		}
		if got := snap.IsInstrumented(); got != tc.wantTrue {
			t.Errorf("%s: IsInstrumented() = %v, want %v", tc.name, got, tc.wantTrue)
		}
	}
}

// keysOf returns the key set of a json.RawMessage map. Local
// equivalent of Object.keys for test diagnostics.
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// containsServerlessTokens is the test-local equivalent of
// strings.Contains used to verify the serverless wire shape without
// colliding with the existing handlers package's contains helper.
func containsServerlessTokens(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Ensure the zap import stays live across builds when the
// per-test handler factory is wired in chunk 5; keeping it
// referenced here means a follow-on chunk doesn't need to re-add
// the import.
var _ = zap.NewNop

// _ = http.StatusOK pins the http package — a follow-up handler
// integration test will use it; keeping the reference here keeps
// the import live without complicating the per-test wiring today.
var _ = http.StatusOK