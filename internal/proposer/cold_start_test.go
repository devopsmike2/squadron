// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// cold_start_test.go — Cold-start latency analysis slice 1 chunk 3
// (v0.89.115, #753 Stream 151). Pins the CheckLambdaColdStart
// detection branch's ShouldFire-gated emit + the per-row exclusion
// filter against §8 / §11 acceptance tests.

// newColdStartRow is the test fixture for the cold-start detection
// branch. Tests vary one field at a time.
func newColdStartRow() ColdStartInventoryRow {
	return ColdStartInventoryRow{
		RecommendationID: "rec-aws-lambda-order-processor",
		Provider:         "aws",
		Surface:          "lambda",
		ResourceID:       "arn:aws:lambda:us-east-1:123:function:order-processor",
		ResourceTFName:   "order_processor",
		Region:           "us-east-1",
	}
}

func newColdStartScope() ColdStartScope {
	return ColdStartScope{
		ConnectionID: "conn-1",
		ScopeID:      "123456789012",
		Region:       "us-east-1",
	}
}

// TestLambdaColdStart_ShouldFire_EmitsRecommendation — the canonical
// happy path: the chunk-2 finding's ShouldFire predicate held, so the
// detection branch emits a lambda-cold-start-baseline draft with the
// per-row reasoning + Terraform snippet from the picker.
func TestLambdaColdStart_ShouldFire_EmitsRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:          true,
		CurrentP95Ms:        4230,
		BaselineP95Ms:       2820,
		Ratio:               1.5,
		CurrentSampleCount:  142,
		BaselineSampleCount: 1086,
	}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft == nil {
		t.Fatalf("draft == nil; want non-nil")
	}
	if draft.Kind != ColdStartRecommendationKind {
		t.Errorf("Kind = %q, want %q", draft.Kind, ColdStartRecommendationKind)
	}
	if draft.Kind != "lambda-cold-start-baseline" {
		t.Errorf("Kind = %q, want lambda-cold-start-baseline (the §8 const)", draft.Kind)
	}
	if !strings.HasSuffix(draft.RecommendationID, ".cold_start") {
		t.Errorf("RecommendationID = %q, want .cold_start suffix", draft.RecommendationID)
	}
	if !strings.Contains(draft.Reasoning, "4230ms") {
		t.Errorf("Reasoning missing current p95 4230ms; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "1.50x") {
		t.Errorf("Reasoning missing ratio 1.50x; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "2820ms") {
		t.Errorf("Reasoning missing baseline 2820ms; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "order-processor") {
		t.Errorf("Reasoning missing resource ARN slug; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "Init script regression") {
		t.Errorf("Reasoning missing three-failure-mode framing cause 1; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "frequency increase") {
		t.Errorf("Reasoning missing three-failure-mode framing cause 2; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "Architecture change") {
		t.Errorf("Reasoning missing three-failure-mode framing cause 3; got: %s", draft.Reasoning)
	}
	if draft.Terraform == "" {
		t.Errorf("Terraform empty; want the picker's snippet")
	}
	if !strings.Contains(draft.Terraform, "aws_lambda_provisioned_concurrency_config") {
		t.Errorf("Terraform missing the provisioned concurrency resource; got: %s", draft.Terraform)
	}
	if draft.ResourceID != row.ResourceID {
		t.Errorf("ResourceID = %q, want %q", draft.ResourceID, row.ResourceID)
	}
}

// TestLambdaColdStart_DoesNotShouldFire_NoRecommendation — when the
// chunk-2 finding's ShouldFire predicate didn't hold (any of the three
// sub-rules — ratio, floor, baseline samples), the detection branch
// emits nothing.
func TestLambdaColdStart_DoesNotShouldFire_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:    false,
		CurrentP95Ms:  4230,
		BaselineP95Ms: 2820,
		Ratio:         1.5,
	}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on !ShouldFire; got: %+v", draft)
	}
}

// TestLambdaColdStart_NilFinding_NoRecommendation — a row with no
// chunk-2 finding (e.g. a Lambda younger than the first scan window,
// or a row the scanner couldn't query) produces no draft.
func TestLambdaColdStart_NilFinding_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	draft, err := CheckLambdaColdStart(context.Background(), row, nil, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on nil finding; got: %+v", draft)
	}
}

// TestLambdaColdStart_NonLambdaSurface_NoRecommendation — slice 1
// covers AWS Lambda only. Rows on cloudrun / cloudfunc / azfunc /
// ocifunc surfaces emit nothing even when ShouldFire is true.
func TestLambdaColdStart_NonLambdaSurface_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	row.Surface = "cloudrun"
	finding := &ColdStartDetectionFinding{ShouldFire: true}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on non-lambda surface; got: %+v", draft)
	}
}

// TestLambdaColdStart_ExcludedRow_NoRecommendation — the operator's
// per-row "Don't propose this again" affordance is honored: when the
// exclusion store carries a row with RecommendationID matching the
// per-row.cold_start suffix, no draft emits.
func TestLambdaColdStart_ExcludedRow_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:          true,
		CurrentP95Ms:        4230,
		BaselineP95Ms:       2820,
		Ratio:               1.5,
		BaselineSampleCount: 1000,
	}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{
			RecommendationID:   row.RecommendationID + ".cold_start",
			RecommendationKind: ColdStartRecommendationKind,
		},
	}}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), exclusions)
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on excluded row; got: %+v", draft)
	}
}

// TestLambdaColdStart_ExcludedByKindOnly_NoRecommendation — the
// kind-only exclusion marker (RecommendationID="" + RecommendationKind
// set) covers the whole scope. Mirrors the trace-emission +
// span-quality posture.
func TestLambdaColdStart_ExcludedByKindOnly_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:          true,
		CurrentP95Ms:        4230,
		BaselineP95Ms:       2820,
		Ratio:               1.5,
		BaselineSampleCount: 1000,
	}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{
			RecommendationID:   "",
			RecommendationKind: ColdStartRecommendationKind,
		},
	}}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), exclusions)
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on kind-only exclusion; got: %+v", draft)
	}
}

// TestLambdaColdStartBatch_AccumulatesDrafts — the batch wrapper runs
// CheckLambdaColdStart over multiple rows + per-row findings, returns
// the accumulated drafts in order.
func TestLambdaColdStartBatch_AccumulatesDrafts(t *testing.T) {
	row1 := newColdStartRow()
	row1.RecommendationID = "rec-1"
	row2 := newColdStartRow()
	row2.RecommendationID = "rec-2"
	rows := []ColdStartInventoryRow{row1, row2}
	findings := []*ColdStartDetectionFinding{
		{ShouldFire: true, CurrentP95Ms: 1000, BaselineP95Ms: 500, Ratio: 2.0, BaselineSampleCount: 100},
		nil, // simulates "no observation yet for row 2"
	}
	drafts, errs := CheckLambdaColdStartBatch(context.Background(), rows, findings, newColdStartScope(), &fakeExclusionStore{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1 (only row 1 fires)", len(drafts))
	}
	if !strings.HasPrefix(drafts[0].RecommendationID, "rec-1") {
		t.Errorf("draft.RecommendationID = %q, want rec-1 prefix", drafts[0].RecommendationID)
	}
}

// ---------------------------------------------------------------------
// Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream
// 157) — per-cloud detection branch tests siblings to the slice 1
// CheckLambdaColdStart tests above.
// ---------------------------------------------------------------------

func newCloudRunRow() ColdStartInventoryRow {
	return ColdStartInventoryRow{
		RecommendationID: "rec-gcp-cloudrun-checkout",
		Provider:         "gcp",
		Surface:          "cloudrun",
		ResourceID:       "projects/my-proj/locations/us-central1/services/checkout-svc",
		ResourceTFName:   "checkout_svc",
		Region:           "us-central1",
	}
}

func newCloudFuncRow() ColdStartInventoryRow {
	return ColdStartInventoryRow{
		RecommendationID: "rec-gcp-cloudfunc-resize",
		Provider:         "gcp",
		Surface:          "cloudfunc",
		ResourceID:       "projects/my-proj/locations/us-central1/functions/image-resize",
		ResourceTFName:   "image_resize",
		Region:           "us-central1",
	}
}

func newAzureFuncRow() ColdStartInventoryRow {
	return ColdStartInventoryRow{
		RecommendationID: "rec-azure-azfunc-payments",
		Provider:         "azure",
		Surface:          "azfunc",
		ResourceID:       "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Web/sites/payments-fn",
		ResourceTFName:   "payments_func",
		Region:           "eastus",
	}
}

func newOCIFuncRow() ColdStartInventoryRow {
	return ColdStartInventoryRow{
		RecommendationID: "rec-oci-ocifunc-ingest",
		Provider:         "oci",
		Surface:          "ocifunc",
		ResourceID:       "ocid1.fnfunc.oc1.iad.aaaa",
		ResourceTFName:   "ingest_worker",
		Region:           "us-ashburn-1",
	}
}

// TestCheckCloudRunColdStart_ShouldFire_EmitsRecommendation — Cloud
// Run happy path. The detection finding fires, the picker emits the
// minScale annotation pattern, the reasoning carries the warm-path
// caveat.
func TestCheckCloudRunColdStart_ShouldFire_EmitsRecommendation(t *testing.T) {
	row := newCloudRunRow()
	finding := &ColdStartDetectionFindingPerCloud{
		ShouldFire:          true,
		CurrentP95Ms:        1800,
		BaselineP95Ms:       1000,
		Ratio:               1.8,
		CurrentSampleCount:  120,
		BaselineSampleCount: 840,
	}
	draft, err := CheckCloudRunColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckCloudRunColdStart: %v", err)
	}
	if draft == nil {
		t.Fatal("draft == nil; want non-nil")
	}
	if draft.Kind != ColdStartRecommendationKindCloudRun {
		t.Errorf("Kind = %q, want %q", draft.Kind, ColdStartRecommendationKindCloudRun)
	}
	if draft.Kind != "cloudrun-cold-start-baseline" {
		t.Errorf("Kind = %q, want cloudrun-cold-start-baseline", draft.Kind)
	}
	if !strings.HasSuffix(draft.RecommendationID, ".cold_start") {
		t.Errorf("RecommendationID = %q, want .cold_start suffix", draft.RecommendationID)
	}
	for _, want := range []string{"1800ms", "1.80x", "1000ms", "warm-path", "minScale"} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, draft.Reasoning)
		}
	}
	if !strings.Contains(draft.Terraform, "autoscaling.knative.dev/minScale") {
		t.Errorf("Terraform missing minScale annotation; got: %s", draft.Terraform)
	}
}

// TestCheckCloudFunctionsColdStart_ShouldFire_EmitsRecommendation —
// Cloud Functions happy path.
func TestCheckCloudFunctionsColdStart_ShouldFire_EmitsRecommendation(t *testing.T) {
	row := newCloudFuncRow()
	finding := &ColdStartDetectionFindingPerCloud{
		ShouldFire:          true,
		CurrentP95Ms:        2400,
		BaselineP95Ms:       1500,
		Ratio:               1.6,
		CurrentSampleCount:  90,
		BaselineSampleCount: 720,
	}
	draft, err := CheckCloudFunctionsColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckCloudFunctionsColdStart: %v", err)
	}
	if draft == nil {
		t.Fatal("draft == nil; want non-nil")
	}
	if draft.Kind != ColdStartRecommendationKindCloudFunc {
		t.Errorf("Kind = %q, want %q", draft.Kind, ColdStartRecommendationKindCloudFunc)
	}
	if draft.Kind != "cloudfunc-cold-start-baseline" {
		t.Errorf("Kind = %q, want cloudfunc-cold-start-baseline", draft.Kind)
	}
	for _, want := range []string{"2400ms", "1.60x", "1500ms", "execution_times", "min_instance_count"} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, draft.Reasoning)
		}
	}
	if !strings.Contains(draft.Terraform, "min_instance_count = 1") {
		t.Errorf("Terraform missing min_instance_count; got: %s", draft.Terraform)
	}
}

// TestCheckAzureFunctionsColdStart_ShouldFire_EmitsRecommendation —
// Azure Functions happy path WITHOUT the UsedFallback signal. The
// reasoning text omits the fallback note when the runtime emits
// IsAfterColdStart.
func TestCheckAzureFunctionsColdStart_ShouldFire_EmitsRecommendation(t *testing.T) {
	row := newAzureFuncRow()
	finding := &ColdStartDetectionFindingPerCloud{
		ShouldFire:          true,
		CurrentP95Ms:        3200,
		BaselineP95Ms:       2000,
		Ratio:               1.6,
		CurrentSampleCount:  110,
		BaselineSampleCount: 770,
		UsedFallback:        false,
	}
	draft, err := CheckAzureFunctionsColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckAzureFunctionsColdStart: %v", err)
	}
	if draft == nil {
		t.Fatal("draft == nil; want non-nil")
	}
	if draft.Kind != ColdStartRecommendationKindAzureFunc {
		t.Errorf("Kind = %q, want %q", draft.Kind, ColdStartRecommendationKindAzureFunc)
	}
	if draft.Kind != "azfunc-cold-start-baseline" {
		t.Errorf("Kind = %q, want azfunc-cold-start-baseline", draft.Kind)
	}
	for _, want := range []string{"3200ms", "1.60x", "2000ms", "Premium Plan", "WEBSITE_USE_PLACEHOLDER"} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, draft.Reasoning)
		}
	}
	if strings.Contains(draft.Reasoning, "INFORMATIONAL NOTE") {
		t.Errorf("Reasoning should NOT include fallback note when UsedFallback=false; got: %s", draft.Reasoning)
	}
	if strings.Contains(draft.Reasoning, "IsAfterColdStart dimension") {
		t.Errorf("Reasoning should NOT mention IsAfterColdStart fallback when UsedFallback=false; got: %s", draft.Reasoning)
	}
}

// TestCheckAzureFunctionsColdStart_UsedFallback_RecorderInformationalNote
// — when the Azure detection finding carries UsedFallback=true, the
// reasoning text MUST add the informational note explaining that
// Squadron fell back to an unfiltered query because the Function
// App's runtime doesn't emit IsAfterColdStart. The note tells the
// operator the metric is not cold-start-isolated.
func TestCheckAzureFunctionsColdStart_UsedFallback_RecorderInformationalNote(t *testing.T) {
	row := newAzureFuncRow()
	finding := &ColdStartDetectionFindingPerCloud{
		ShouldFire:          true,
		CurrentP95Ms:        2800,
		BaselineP95Ms:       1700,
		Ratio:               1.65,
		CurrentSampleCount:  100,
		BaselineSampleCount: 700,
		UsedFallback:        true,
	}
	draft, err := CheckAzureFunctionsColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckAzureFunctionsColdStart: %v", err)
	}
	if draft == nil {
		t.Fatal("draft == nil; want non-nil")
	}
	for _, want := range []string{
		"INFORMATIONAL NOTE",
		"IsAfterColdStart dimension",
		"runtime",
		"2023+",
		"unfiltered",
		"not cold-start-isolated",
	} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing fallback note marker %q; got: %s", want, draft.Reasoning)
		}
	}
}

// TestCheckOCIFunctionsColdStart_SkippedWhenNoColdStarts_NoRecommendation
// — when the OCI finding's ShouldFire was set to true but Skipped
// is also true, the defensive gate rejects the draft. In production
// the chunk-3 ShouldFireRecommendation predicate already gates on
// !Skipped, but this test pins the defensive belt-and-suspenders
// behavior at the proposer boundary.
func TestCheckOCIFunctionsColdStart_SkippedWhenNoColdStarts_NoRecommendation(t *testing.T) {
	row := newOCIFuncRow()
	finding := &ColdStartDetectionFindingPerCloud{
		// Production wiring would never reach this combination
		// because the chunk-3 ShouldFireRecommendation predicate
		// short-circuits on Skipped. Test pins defensive behavior.
		ShouldFire: true,
		Skipped:    true,
	}
	draft, err := CheckOCIFunctionsColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckOCIFunctionsColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil when Skipped=true; got: %+v", draft)
	}

	// Also pin: when ShouldFire is false (the production case),
	// no draft emits even when Skipped is false.
	notFiring := &ColdStartDetectionFindingPerCloud{
		ShouldFire: false,
	}
	draft, err = CheckOCIFunctionsColdStart(context.Background(), row, notFiring, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckOCIFunctionsColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil when ShouldFire=false; got: %+v", draft)
	}
}

// TestCheckOCIFunctionsColdStart_ShouldFire_EmitsRecommendation — OCI
// happy path. The reasoning text surfaces the cold_start_count
// honesty caveat: function_duration is not cold-start-isolated.
func TestCheckOCIFunctionsColdStart_ShouldFire_EmitsRecommendation(t *testing.T) {
	row := newOCIFuncRow()
	finding := &ColdStartDetectionFindingPerCloud{
		ShouldFire:            true,
		CurrentP95Ms:          2100,
		BaselineP95Ms:         1300,
		Ratio:                 1.62,
		CurrentSampleCount:    80,
		BaselineSampleCount:   560,
		CurrentColdStartCount: 14,
	}
	draft, err := CheckOCIFunctionsColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckOCIFunctionsColdStart: %v", err)
	}
	if draft == nil {
		t.Fatal("draft == nil; want non-nil")
	}
	if draft.Kind != ColdStartRecommendationKindOCIFunc {
		t.Errorf("Kind = %q, want %q", draft.Kind, ColdStartRecommendationKindOCIFunc)
	}
	if draft.Kind != "ocifunc-cold-start-baseline" {
		t.Errorf("Kind = %q, want ocifunc-cold-start-baseline", draft.Kind)
	}
	for _, want := range []string{
		"2100ms",
		"1.62x",
		"1300ms",
		"cold_start_count=14",
		"function_duration",
		"not cold-start-isolated",
		"WARMUP_DELAY",
		"preview",
	} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, draft.Reasoning)
		}
	}
}

// TestAllFourClouds_ExclusionRespected — the per-row exclusion check
// honors the same .cold_start suffix convention slice 1 introduced,
// applied across all four new helpers. Pins the cross-cloud exclusion
// invariant.
func TestAllFourClouds_ExclusionRespected(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  ColdStartInventoryRow
		kind string
		call func(ctx context.Context, row ColdStartInventoryRow, f *ColdStartDetectionFindingPerCloud, s ColdStartScope, e ColdStartExclusionStore) (*ColdStartRecommendationDraft, error)
	}{
		{"cloudrun", newCloudRunRow(), ColdStartRecommendationKindCloudRun, CheckCloudRunColdStart},
		{"cloudfunc", newCloudFuncRow(), ColdStartRecommendationKindCloudFunc, CheckCloudFunctionsColdStart},
		{"azfunc", newAzureFuncRow(), ColdStartRecommendationKindAzureFunc, CheckAzureFunctionsColdStart},
		{"ocifunc", newOCIFuncRow(), ColdStartRecommendationKindOCIFunc, CheckOCIFunctionsColdStart},
	} {
		t.Run(tc.name+"_per_row", func(t *testing.T) {
			finding := &ColdStartDetectionFindingPerCloud{
				ShouldFire:          true,
				CurrentP95Ms:        4000,
				BaselineP95Ms:       2000,
				Ratio:               2.0,
				BaselineSampleCount: 800,
			}
			exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{{
				RecommendationID:   tc.row.RecommendationID + ".cold_start",
				RecommendationKind: tc.kind,
			}}}
			draft, err := tc.call(context.Background(), tc.row, finding, newColdStartScope(), exclusions)
			if err != nil {
				t.Fatalf("%s call: %v", tc.name, err)
			}
			if draft != nil {
				t.Errorf("%s: draft != nil for per-row excluded; got: %+v", tc.name, draft)
			}
		})
		t.Run(tc.name+"_kind_only", func(t *testing.T) {
			finding := &ColdStartDetectionFindingPerCloud{
				ShouldFire:          true,
				CurrentP95Ms:        4000,
				BaselineP95Ms:       2000,
				Ratio:               2.0,
				BaselineSampleCount: 800,
			}
			exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{{
				RecommendationID:   "",
				RecommendationKind: tc.kind,
			}}}
			draft, err := tc.call(context.Background(), tc.row, finding, newColdStartScope(), exclusions)
			if err != nil {
				t.Fatalf("%s call: %v", tc.name, err)
			}
			if draft != nil {
				t.Errorf("%s: draft != nil for kind-only excluded; got: %+v", tc.name, draft)
			}
		})
	}
}

// TestPerCloudColdStart_WrongSurface_NoRecommendation — each per-cloud
// helper rejects rows on a non-matching surface, mirroring the slice
// 1 CheckLambdaColdStart guard.
func TestPerCloudColdStart_WrongSurface_NoRecommendation(t *testing.T) {
	finding := &ColdStartDetectionFindingPerCloud{
		ShouldFire:          true,
		CurrentP95Ms:        3000,
		BaselineP95Ms:       1500,
		Ratio:               2.0,
		BaselineSampleCount: 500,
	}
	// A Cloud Run row passed to the Azure helper should NOT produce
	// a draft (and vice-versa).
	cloudRunRow := newCloudRunRow()
	draft, _ := CheckAzureFunctionsColdStart(context.Background(), cloudRunRow, finding, newColdStartScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Errorf("CheckAzureFunctionsColdStart accepted cloudrun row; got: %+v", draft)
	}
	draft, _ = CheckOCIFunctionsColdStart(context.Background(), cloudRunRow, finding, newColdStartScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Errorf("CheckOCIFunctionsColdStart accepted cloudrun row; got: %+v", draft)
	}
	azureRow := newAzureFuncRow()
	draft, _ = CheckCloudRunColdStart(context.Background(), azureRow, finding, newColdStartScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Errorf("CheckCloudRunColdStart accepted azfunc row; got: %+v", draft)
	}
}
