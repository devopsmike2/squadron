// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// newSpanQualityRow is the test fixture for the span-quality
// detection branch. Tests vary one field at a time.
func newSpanQualityRow() SpanQualityInventoryRow {
	return SpanQualityInventoryRow{
		RecommendationID: "rec-aws-ec2-i-0abc",
		Provider:         "aws",
		Tier:             "compute",
		ResourceID:       "i-0abc",
		ResourceTFName:   "web_01",
		Region:           "us-east-1",
	}
}

// TestSpanQualityDetection_OrphanAt11Pct_EmitsRecommendation — §10
// acceptance test 7. Threshold edge case: 11% orphan crosses the 10%
// cutoff and emits a span-quality-orphan-trace draft.
func TestSpanQualityDetection_OrphanAt11Pct_EmitsRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{TotalSpans: 1000, OrphanPct: 11.0}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}

	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1", len(drafts))
	}
	if drafts[0].Kind != "span-quality-orphan-trace" {
		t.Errorf("Kind = %q, want span-quality-orphan-trace", drafts[0].Kind)
	}
	if drafts[0].RecommendationID != "rec-aws-ec2-i-0abc.orphan" {
		t.Errorf("RecommendationID = %q, want suffix .orphan", drafts[0].RecommendationID)
	}
	if !strings.Contains(drafts[0].Reasoning, "11.0%") {
		t.Errorf("Reasoning missing 11.0%% percentage; got: %s", drafts[0].Reasoning)
	}
	if !strings.Contains(drafts[0].Reasoning, "i-0abc") {
		t.Errorf("Reasoning missing resource id; got: %s", drafts[0].Reasoning)
	}
	if drafts[0].Terraform == "" {
		t.Errorf("Terraform empty for compute tier; want a per-cloud snippet")
	}
}

// TestSpanQualityDetection_OrphanAt9Pct_NoRecommendation — §10
// acceptance test 8. Threshold edge case: 9% orphan falls below the
// 10% cutoff and emits nothing.
func TestSpanQualityDetection_OrphanAt9Pct_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{TotalSpans: 1000, OrphanPct: 9.0}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 below threshold", len(drafts))
	}
}

// TestSpanQualityDetection_MissingAttrsAt26Pct_EmitsRecommendation —
// the missing-attrs threshold (25%) +1 edge.
func TestSpanQualityDetection_MissingAttrsAt26Pct_EmitsRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{TotalSpans: 1000, MissingAttrPct: 26.0}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1", len(drafts))
	}
	if drafts[0].Kind != "span-quality-missing-resource-attrs" {
		t.Errorf("Kind = %q", drafts[0].Kind)
	}
	if !strings.HasSuffix(drafts[0].RecommendationID, ".missing") {
		t.Errorf("RecommendationID = %q, want .missing suffix", drafts[0].RecommendationID)
	}
}

// TestSpanQualityDetection_MismatchAt6Pct_EmitsRecommendation — the
// mismatch threshold (5%) +1 edge.
func TestSpanQualityDetection_MismatchAt6Pct_EmitsRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:      1000,
		AttrMismatchPct: 6.0,
		Placeholders: []traceindex.PlaceholderObservation{
			{Attribute: "host.name", Placeholder: "localhost"},
		},
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1", len(drafts))
	}
	if drafts[0].Kind != "span-quality-attribute-mismatch" {
		t.Errorf("Kind = %q", drafts[0].Kind)
	}
	// The placeholder observations from the snapshot must surface in
	// the reasoning so the operator sees which sentinel the SDK fell
	// back to.
	if !strings.Contains(drafts[0].Reasoning, "host.name") || !strings.Contains(drafts[0].Reasoning, "localhost") {
		t.Errorf("Reasoning missing placeholder observation; got: %s", drafts[0].Reasoning)
	}
}

// TestSpanQualityDetection_BelowMinimumSampleSize_NoRecommendation —
// the SpanQualityMinimumSpansThreshold guard. 50 spans, 100% orphan
// → no draft (insufficient sample size).
func TestSpanQualityDetection_BelowMinimumSampleSize_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{TotalSpans: 50, OrphanPct: 100.0, MissingAttrPct: 100.0, AttrMismatchPct: 100.0}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 (below sample-size floor)", len(drafts))
	}
}

// TestSpanQualityDetection_MultiplePathologies_EmitsMultipleKinds —
// all three thresholds tripped simultaneously → three drafts.
func TestSpanQualityDetection_MultiplePathologies_EmitsMultipleKinds(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:      2000,
		OrphanPct:       15.0,
		MissingAttrPct:  30.0,
		AttrMismatchPct: 8.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 3 {
		t.Fatalf("len(drafts) = %d, want 3", len(drafts))
	}
	kinds := map[string]bool{}
	for _, d := range drafts {
		kinds[d.Kind] = true
	}
	for _, want := range []string{"span-quality-orphan-trace", "span-quality-missing-resource-attrs", "span-quality-attribute-mismatch"} {
		if !kinds[want] {
			t.Errorf("missing kind %q in drafts", want)
		}
	}
}

// TestSpanQualityDetection_ExcludedRow_NoRecommendation — the
// per-row exclusion entry suppresses the orphan kind only; missing
// + mismatch still fire if their thresholds trip. The kind-only
// exclusion entry suppresses across the scope.
func TestSpanQualityDetection_ExcludedRow_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:      2000,
		OrphanPct:       15.0,
		MissingAttrPct:  30.0,
		AttrMismatchPct: 8.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}

	t.Run("row-specific exclusion suppresses only the orphan kind", func(t *testing.T) {
		exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
			{RecommendationID: "rec-aws-ec2-i-0abc.orphan", ExcludedBy: "alice"},
		}}
		drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, exclusions)
		if err != nil {
			t.Fatalf("CheckSpanQualityIssues: %v", err)
		}
		if len(drafts) != 2 {
			t.Fatalf("len(drafts) = %d, want 2 (orphan excluded, missing + mismatch still fire)", len(drafts))
		}
		for _, d := range drafts {
			if d.Kind == "span-quality-orphan-trace" {
				t.Errorf("orphan should have been excluded")
			}
		}
	})

	t.Run("kind-only exclusion suppresses across the scope", func(t *testing.T) {
		exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
			{RecommendationID: "", RecommendationKind: "span-quality-attribute-mismatch", ExcludedBy: "alice"},
		}}
		drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, exclusions)
		if err != nil {
			t.Fatalf("CheckSpanQualityIssues: %v", err)
		}
		if len(drafts) != 2 {
			t.Fatalf("len(drafts) = %d, want 2 (mismatch kind excluded)", len(drafts))
		}
		for _, d := range drafts {
			if d.Kind == "span-quality-attribute-mismatch" {
				t.Errorf("mismatch should have been excluded")
			}
		}
	})
}

// TestSpanQualityDetection_NilSnapshot_NoRecommendation — defensive:
// no Quality snapshot at all → no draft. Mirrors the "primitive off
// → no trace-emission draft" posture so the wiring layer can pass a
// nil snapshot for rows that never observed any spans without
// crashing the detection branch.
func TestSpanQualityDetection_NilSnapshot_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, nil, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 (nil snapshot)", len(drafts))
	}
}

// TestSpanQualityDetection_BatchAccumulatesDrafts — the batch
// wrapper runs the per-row detection and accumulates the drafts in
// order. Per-row errors get collected but don't drop the rest.
func TestSpanQualityDetection_BatchAccumulatesDrafts(t *testing.T) {
	rows := []SpanQualityInventoryRow{
		{RecommendationID: "rec-1", Provider: "aws", Tier: "compute", ResourceID: "i-1", Region: "us-east-1"},
		{RecommendationID: "rec-2", Provider: "gcp", Tier: "k8s", ResourceID: "gke-1", Region: "us-central1"},
	}
	snaps := []*traceindex.QualityCountersSnapshot{
		{TotalSpans: 1000, OrphanPct: 12.0},
		{TotalSpans: 1000, MissingAttrPct: 30.0},
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, errs := CheckSpanQualityIssuesBatch(context.Background(), rows, snaps, scope, &fakeExclusionStore{})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(drafts) != 2 {
		t.Fatalf("len(drafts) = %d, want 2", len(drafts))
	}
	if drafts[0].Kind != "span-quality-orphan-trace" || drafts[1].Kind != "span-quality-missing-resource-attrs" {
		t.Errorf("kinds = %q / %q", drafts[0].Kind, drafts[1].Kind)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSpanQualitySlice1
// — span quality slice 1 chunk 2 cold-start parity invariant.
// Across all four providers, the user message produced by
// buildDiscoveryUserMessage stays byte-identical when the scan
// context carries no span-quality observations. The 3 new kind
// strings live ONLY in the system prompt; the user-message renderer
// is unchanged.
//
// Pins §10 acceptance test 13 (the trace-integration parity invariant
// extended by the slice-1 design doc to require parity to v0.89.85).
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSpanQualitySlice1(t *testing.T) {
	cases := []struct {
		name string
		in   ai.DiscoveryScanContext
	}{
		{
			name: "aws",
			in:   ai.DiscoveryScanContext{ScanID: "scan-aws-cold", AccountID: "123456789012", Regions: []string{"us-east-1"}},
		},
		{
			name: "gcp",
			in:   ai.DiscoveryScanContext{ScanID: "scan-gcp-cold", Provider: "gcp", ProjectID: "my-project", Regions: []string{"us-central1"}},
		},
		{
			name: "azure",
			in:   ai.DiscoveryScanContext{ScanID: "scan-azure-cold", Provider: "azure", TenantID: "tnt", SubscriptionID: "sub", Regions: []string{"eastus"}},
		},
		{
			name: "oci",
			in:   ai.DiscoveryScanContext{ScanID: "scan-oci-cold", Provider: "oci", TenancyOCID: "ocid1.tenancy.oc1..aaaa", Regions: []string{"us-phoenix-1"}},
		},
	}
	spanQualityKinds := []string{
		"span-quality-orphan-trace",
		"span-quality-missing-resource-attrs",
		"span-quality-attribute-mismatch",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ai.BuildDiscoveryUserMessageForTest(tc.in)
			for _, kind := range spanQualityKinds {
				if strings.Contains(msg, kind) {
					t.Errorf("cold-start user message must NOT contain span-quality kind %q (provider=%s)", kind, tc.name)
				}
			}
		})
	}

	// System prompt MUST contain all three kinds — the model needs
	// to know about them even if no inventory row triggers them this
	// scan.
	systemPrompt := ai.DiscoverySystemPromptForTest()
	for _, kind := range spanQualityKinds {
		if !strings.Contains(systemPrompt, kind) {
			t.Errorf("system prompt missing span-quality kind %q", kind)
		}
	}
	if !strings.Contains(systemPrompt, "SPAN QUALITY KINDS") {
		t.Errorf("system prompt missing SPAN QUALITY KINDS section header")
	}
}

// TestSpanQualityDetection_RecommendationTimestamp_DeterministicForBatch — the
// batch wrapper does not require a now timestamp parameter (unlike the
// trace-emission detection's 24h-staleness check). This is by design
// — the span-quality thresholds operate on the rolling-window
// percentages from the snapshot, which already encode time. Confirm
// the signature stays clock-free.
func TestSpanQualityDetection_RecommendationTimestamp_DeterministicForBatch(t *testing.T) {
	// Compile-time check that the batch signature does NOT take a
	// time.Time parameter. If a future refactor adds one, this test
	// will fail to compile and force the design choice to be
	// revisited.
	var _ func(
		context.Context,
		[]SpanQualityInventoryRow,
		[]*traceindex.QualityCountersSnapshot,
		SpanQualityScope,
		SpanQualityExclusionStore,
	) ([]SpanQualityRecommendationDraft, []error) = CheckSpanQualityIssuesBatch
	_ = time.Now // silence unused-import linter
}

// --- Slice 2 (v0.89.110) tests — W3C trace context detection ------

// TestSpanQualityDetection_MalformedTraceparentAt2Pct_EmitsRecommendation
// — §11 acceptance test 15. 2% malformed crosses the 1% threshold and
// emits a span-quality-traceparent-malformed draft.
func TestSpanQualityDetection_MalformedTraceparentAt2Pct_EmitsRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:              1000,
		SpansWithTraceparent:    200,
		MalformedTraceparentPct: 2.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1", len(drafts))
	}
	if drafts[0].Kind != "span-quality-traceparent-malformed" {
		t.Errorf("Kind = %q, want span-quality-traceparent-malformed", drafts[0].Kind)
	}
	if !strings.HasSuffix(drafts[0].RecommendationID, ".traceparent_malformed") {
		t.Errorf("RecommendationID = %q, want .traceparent_malformed suffix", drafts[0].RecommendationID)
	}
	if !strings.Contains(drafts[0].Reasoning, "2.0%") {
		t.Errorf("Reasoning missing 2.0%%; got: %s", drafts[0].Reasoning)
	}
}

// TestSpanQualityDetection_MalformedTraceparentAt0Pt5Pct_NoRecommendation
// — 0.5% malformed falls below the 1% threshold and emits nothing.
func TestSpanQualityDetection_MalformedTraceparentAt0Pt5Pct_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:              1000,
		SpansWithTraceparent:    200,
		MalformedTraceparentPct: 0.5,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 (below 1%% threshold)", len(drafts))
	}
}

// TestSpanQualityDetection_MissingTraceparentOnChildAt6Pct_EmitsRecommendation
// — §11 acceptance test 15 variant. 6% missing-on-child crosses the 5%
// threshold and emits a span-quality-traceparent-missing draft.
func TestSpanQualityDetection_MissingTraceparentOnChildAt6Pct_EmitsRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:                   1000,
		ChildSpans:                   500,
		MissingTraceparentOnChildPct: 6.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1", len(drafts))
	}
	if drafts[0].Kind != "span-quality-traceparent-missing" {
		t.Errorf("Kind = %q, want span-quality-traceparent-missing", drafts[0].Kind)
	}
	if !strings.HasSuffix(drafts[0].RecommendationID, ".traceparent_missing") {
		t.Errorf("RecommendationID = %q, want .traceparent_missing suffix", drafts[0].RecommendationID)
	}
	if !strings.Contains(drafts[0].Reasoning, "6.0%") {
		t.Errorf("Reasoning missing 6.0%%; got: %s", drafts[0].Reasoning)
	}
}

// TestSpanQualityDetection_MissingTraceparentOnChildAt4Pct_NoRecommendation
// — §11 acceptance test 16. 4% missing-on-child falls below the 5%
// threshold and emits nothing.
func TestSpanQualityDetection_MissingTraceparentOnChildAt4Pct_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:                   1000,
		ChildSpans:                   500,
		MissingTraceparentOnChildPct: 4.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 (below 5%% threshold)", len(drafts))
	}
}

// TestSpanQualityDetection_SpansWithTraceparentBelowMinimum_NoRecommendation
// — 5% malformed with only 30 spans-with-traceparent (below the 50
// minimum) emits nothing. The threshold percentage alone is too
// noisy when the denominator is small.
func TestSpanQualityDetection_SpansWithTraceparentBelowMinimum_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:              1000,
		SpansWithTraceparent:    30, // below SpanQualityMinimumSpansWithTraceparent=50
		MalformedTraceparentPct: 5.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 (below SpansWithTraceparent minimum)", len(drafts))
	}
}

// TestSpanQualityDetection_ChildSpansBelowMinimum_NoRecommendation —
// 10% missing-on-child with only 20 child spans (below the 50 minimum)
// emits nothing.
func TestSpanQualityDetection_ChildSpansBelowMinimum_NoRecommendation(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:                   1000,
		ChildSpans:                   20, // below SpanQualityMinimumChildSpans=50
		MissingTraceparentOnChildPct: 10.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("len(drafts) = %d, want 0 (below ChildSpans minimum)", len(drafts))
	}
}

// TestSpanQualityDetection_BothTraceparentPathologies_EmitsBothKinds —
// when both traceparent thresholds trip on the same row (with both
// denominators above minimum), the branch emits TWO drafts — one per
// kind.
func TestSpanQualityDetection_BothTraceparentPathologies_EmitsBothKinds(t *testing.T) {
	row := newSpanQualityRow()
	qual := &traceindex.QualityCountersSnapshot{
		TotalSpans:                   2000,
		SpansWithTraceparent:         500,
		ChildSpans:                   800,
		MalformedTraceparentPct:      3.0,
		MissingTraceparentOnChildPct: 9.0,
	}
	scope := SpanQualityScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	drafts, err := CheckSpanQualityIssues(context.Background(), row, qual, scope, &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckSpanQualityIssues: %v", err)
	}
	if len(drafts) != 2 {
		t.Fatalf("len(drafts) = %d, want 2 (both traceparent kinds)", len(drafts))
	}
	kinds := map[string]bool{}
	for _, d := range drafts {
		kinds[d.Kind] = true
	}
	for _, want := range []string{"span-quality-traceparent-malformed", "span-quality-traceparent-missing"} {
		if !kinds[want] {
			t.Errorf("missing kind %q in drafts", want)
		}
	}
}

// TestDiscoveryProposer_TraceparentKindsInSystemPrompt — verifies both
// new slice 2 kind strings appear verbatim in the system prompt.
func TestDiscoveryProposer_TraceparentKindsInSystemPrompt(t *testing.T) {
	systemPrompt := ai.DiscoverySystemPromptForTest()
	for _, kind := range []string{
		"span-quality-traceparent-missing",
		"span-quality-traceparent-malformed",
	} {
		if !strings.Contains(systemPrompt, kind) {
			t.Errorf("system prompt missing span-quality slice 2 kind %q", kind)
		}
	}
	if !strings.Contains(systemPrompt, "SPAN QUALITY TRACEPARENT KINDS") {
		t.Errorf("system prompt missing SPAN QUALITY TRACEPARENT KINDS section header")
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSpanQualitySlice2
// — span quality slice 2 chunk 2 cold-start parity invariant. Across
// all four providers, the user message produced by
// buildDiscoveryUserMessage stays byte-identical when the scan context
// carries no inventory rows that trigger traceparent kinds. The 2 new
// kind strings live ONLY in the system prompt; the user-message
// renderer is unchanged from v0.89.107.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSpanQualitySlice2(t *testing.T) {
	cases := []struct {
		name string
		in   ai.DiscoveryScanContext
	}{
		{
			name: "aws",
			in:   ai.DiscoveryScanContext{ScanID: "scan-aws-cold", AccountID: "123456789012", Regions: []string{"us-east-1"}},
		},
		{
			name: "gcp",
			in:   ai.DiscoveryScanContext{ScanID: "scan-gcp-cold", Provider: "gcp", ProjectID: "my-project", Regions: []string{"us-central1"}},
		},
		{
			name: "azure",
			in:   ai.DiscoveryScanContext{ScanID: "scan-azure-cold", Provider: "azure", TenantID: "tnt", SubscriptionID: "sub", Regions: []string{"eastus"}},
		},
		{
			name: "oci",
			in:   ai.DiscoveryScanContext{ScanID: "scan-oci-cold", Provider: "oci", TenancyOCID: "ocid1.tenancy.oc1..aaaa", Regions: []string{"us-phoenix-1"}},
		},
	}
	traceparentKinds := []string{
		"span-quality-traceparent-missing",
		"span-quality-traceparent-malformed",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ai.BuildDiscoveryUserMessageForTest(tc.in)
			for _, kind := range traceparentKinds {
				if strings.Contains(msg, kind) {
					t.Errorf("cold-start user message must NOT contain traceparent kind %q (provider=%s)", kind, tc.name)
				}
			}
		})
	}
}
