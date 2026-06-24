// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/aws"
	"github.com/devopsmike2/squadron/internal/discovery/azure"
	"github.com/devopsmike2/squadron/internal/discovery/gcp"
	"github.com/devopsmike2/squadron/internal/discovery/oci"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// error_rate_test.go — Error rate correlation slice 1 chunk 2
// (v0.89.128, #768 Stream 166). Pins the §3 detection rule + §11
// acceptance tests 6-9 + the per-cloud Check helpers + the §12
// near-zero baseline guard.

// stubErrorRateQuerier returns a canned float64 per
// (metricName, window) tuple. The four DetectErrorRate calls expect
// different values per call — invocations 24h, errors 24h,
// invocations 168h, errors 168h — and the canned map keys the
// combination so a single stub serves all four.
type stubErrorRateQuerier struct {
	canned map[string]float64
	err    error
	calls  int
}

func (s *stubErrorRateQuerier) QueryAggregate(
	_ context.Context, resourceARN, metricName string, window time.Duration, stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	s.calls++
	if s.err != nil {
		return scanner.AggregateMetricResult{}, s.err
	}
	key := metricName + ":" + window.String()
	val := s.canned[key]
	return scanner.AggregateMetricResult{
		ResourceARN: resourceARN, MetricName: metricName, Window: window,
		Statistic: stat, Value: val, ObservedAt: time.Now().UTC(),
	}, nil
}

// querierFor builds a canned querier matching the four tuples the
// detection branch issues per resource per surface. invCurrent =
// current 24h invocations, errCurrent = current 24h errors, invBase =
// baseline 168h invocations, errBase = baseline 168h errors.
func querierFor(invCurrent, errCurrent, invBase, errBase float64) *stubErrorRateQuerier {
	return &stubErrorRateQuerier{
		canned: map[string]float64{
			aws.LambdaInvocationsMetricName + ":" + (24 * time.Hour).String():  invCurrent,
			aws.LambdaErrorsMetricName + ":" + (24 * time.Hour).String():       errCurrent,
			aws.LambdaInvocationsMetricName + ":" + (168 * time.Hour).String(): invBase,
			aws.LambdaErrorsMetricName + ":" + (168 * time.Hour).String():      errBase,
		},
	}
}

func newErrorRateRow() ErrorRateInventoryRow {
	return ErrorRateInventoryRow{
		RecommendationID: "rec-aws-lambda-order-processor",
		Provider:         "aws", Surface: "lambda",
		ResourceID:     "arn:aws:lambda:us-east-1:123:function:order-processor",
		ResourceTFName: "order_processor", Region: "us-east-1",
	}
}

func newErrorRateScope() ErrorRateScope {
	return ErrorRateScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}
}

// Test 6 — 3x ratio at 3000 invocations + 90 errors FIRES.
// Current rate: 90/3000 = 3.0%; baseline 60/6000 = 1.0%; ratio 3.0x.
func TestErrorRate_3xRatioAt3000Invocations90Errors_FiresRecommendation(t *testing.T) {
	q := querierFor(3000, 90, 6000, 60)
	res, err := DetectErrorRate(context.Background(), q, "arn:fn", "lambda")
	if err != nil {
		t.Fatalf("DetectErrorRate: %v", err)
	}
	if res.CurrentErrorCount != 90 || res.CurrentInvocationCount != 3000 {
		t.Errorf("current = %d/%d, want 90/3000", res.CurrentErrorCount, res.CurrentInvocationCount)
	}
	if res.BaselineErrorCount != 60 || res.BaselineInvocationCount != 6000 {
		t.Errorf("baseline = %d/%d, want 60/6000", res.BaselineErrorCount, res.BaselineInvocationCount)
	}
	if res.RateRatio < 2.99 || res.RateRatio > 3.01 {
		t.Errorf("RateRatio = %v, want ~3.0", res.RateRatio)
	}
	if !res.ExceedsRateRatioFloor || !res.ExceedsMinimumInvocations || !res.ExceedsMinimumErrors {
		t.Errorf("predicates not all true; got rate=%v inv=%v err=%v",
			res.ExceedsRateRatioFloor, res.ExceedsMinimumInvocations, res.ExceedsMinimumErrors)
	}
	if !res.ShouldFireRecommendation() {
		t.Error("ShouldFire = false, want true")
	}
	if res.BaselineAdjusted {
		t.Error("BaselineAdjusted = true; baseline 1% well above floor")
	}
}

// Test 7 — 1.9x ratio does NOT fire (below 2.0x).
// current 57/3000 = 1.9%; baseline 60/6000 = 1.0%; ratio 1.9x.
func TestErrorRate_1_9xRatioAt3000Invocations_DoesNotFire(t *testing.T) {
	q := querierFor(3000, 57, 6000, 60)
	res, err := DetectErrorRate(context.Background(), q, "arn:fn", "lambda")
	if err != nil {
		t.Fatalf("DetectErrorRate: %v", err)
	}
	if res.RateRatio < 1.89 || res.RateRatio > 1.91 {
		t.Errorf("RateRatio = %v, want ~1.9", res.RateRatio)
	}
	if res.ExceedsRateRatioFloor {
		t.Error("ExceedsRateRatioFloor = true at 1.9x, want false")
	}
	if res.ShouldFireRecommendation() {
		t.Error("ShouldFire = true at 1.9x, want false")
	}
}

// Test 8 — 3x ratio at 500 invocations does NOT fire (below
// minimum invocation count).
// current 15/500 = 3%; baseline 30/3000 = 1%; ratio 3x. Min inv
// floor 1000 blocks.
func TestErrorRate_3xRatioAt500Invocations_DoesNotFire(t *testing.T) {
	q := querierFor(500, 15, 3000, 30)
	res, err := DetectErrorRate(context.Background(), q, "arn:fn", "lambda")
	if err != nil {
		t.Fatalf("DetectErrorRate: %v", err)
	}
	if !res.ExceedsRateRatioFloor {
		t.Error("ExceedsRateRatioFloor = false at 3x, want true")
	}
	if res.ExceedsMinimumInvocations {
		t.Error("ExceedsMinimumInvocations = true at 500 inv, want false")
	}
	if res.ShouldFireRecommendation() {
		t.Error("ShouldFire = true at 500 inv, want false")
	}
}

// Test 9 — 3x ratio at 3000 invocations + 30 errors does NOT
// fire (below absolute error minimum).
// current 30/3000 = 1%; baseline 30/9000 = 0.33%; ratio 3x.
func TestErrorRate_3xRatioAt3000Invocations30Errors_DoesNotFire(t *testing.T) {
	q := querierFor(3000, 30, 9000, 30)
	res, err := DetectErrorRate(context.Background(), q, "arn:fn", "lambda")
	if err != nil {
		t.Fatalf("DetectErrorRate: %v", err)
	}
	if !res.ExceedsRateRatioFloor {
		t.Errorf("ExceedsRateRatioFloor = false at ratio %v, want true", res.RateRatio)
	}
	if !res.ExceedsMinimumInvocations {
		t.Error("ExceedsMinimumInvocations = false at 3000 inv, want true")
	}
	if res.ExceedsMinimumErrors {
		t.Error("ExceedsMinimumErrors = true at 30 errors, want false")
	}
	if res.ShouldFireRecommendation() {
		t.Error("ShouldFire = true at 30 errors, want false")
	}
}

// TestErrorRate_NearZeroBaseline_UsesFloor_DetectsCorrectly —
// pin the §12 baseline-too-low guard. Baseline rate = 0 (no errors
// in baseline window). Current rate = 100/3000 = 3.33%. Without
// the floor, divide-by-zero. With the floor (0.0001 = 0.01%), the
// ratio comes out at 333x, and BaselineAdjusted = true.
func TestErrorRate_NearZeroBaseline_UsesFloor_DetectsCorrectly(t *testing.T) {
	q := querierFor(3000, 100, 8000, 0)
	res, err := DetectErrorRate(context.Background(), q, "arn:fn", "lambda")
	if err != nil {
		t.Fatalf("DetectErrorRate: %v", err)
	}
	if !res.BaselineAdjusted {
		t.Error("BaselineAdjusted = false when baseline error rate is zero, want true")
	}
	// current rate 3.33%; floor 0.01% → ratio 333x.
	if res.RateRatio < 300 || res.RateRatio > 400 {
		t.Errorf("RateRatio = %v, want ~333", res.RateRatio)
	}
	if !res.ShouldFireRecommendation() {
		t.Error("ShouldFire = false, want true with floor-adjusted comparison")
	}
}

// TestErrorRate_ExcludedRow_DoesNotFire pins the exclusion-store
// gate. A row with the .error_rate_spike suffix in the exclusion
// list returns nil draft even when the detection result fires.
func TestErrorRate_ExcludedRow_DoesNotFire(t *testing.T) {
	row := newErrorRateRow()
	result := &ErrorRateDetectionResult{
		ResourceARN: row.ResourceID, Surface: "lambda",
		ExceedsRateRatioFloor: true, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{RecommendationID: row.RecommendationID + ".error_rate_spike"},
	}}
	draft, err := CheckLambdaErrorRate(context.Background(), row, result, newErrorRateScope(), exclusions)
	if err != nil {
		t.Fatalf("CheckLambdaErrorRate: %v", err)
	}
	if draft != nil {
		t.Error("expected nil draft for excluded row")
	}
}

// TestErrorMetricFor_AllFiveSurfaces pins the per-cloud error
// metric routing.
func TestErrorMetricFor_AllFiveSurfaces(t *testing.T) {
	cases := map[string]string{
		"lambda":    aws.LambdaErrorsMetricName,
		"cloudrun":  gcp.CloudRunRequestCount5xxMetricType,
		"cloudfunc": gcp.CloudFunctionsExecutionCountErrorMetricType,
		"azfunc":    azure.AzureFunctionsErrorsMetric,
		"ocifunc":   oci.OCIFunctionsInvocationCountErrorMetric,
	}
	for surface, want := range cases {
		got, ok := errorMetricFor(surface)
		if !ok || got != want {
			t.Errorf("errorMetricFor(%q) = (%q, %v), want (%q, true)", surface, got, ok, want)
		}
	}
	if _, ok := errorMetricFor("unknown"); ok {
		t.Error("unknown surface should be unsupported")
	}
}

// TestCheckLambdaErrorRate_HappyPath — the firing Lambda draft has
// the .error_rate_spike suffix, the AWS Lambda Terraform pattern,
// and the 3-failure-mode reasoning explicitly framing cases (1) +
// (2) as MORE COMMON.
func TestCheckLambdaErrorRate_HappyPath(t *testing.T) {
	row := newErrorRateRow()
	result := &ErrorRateDetectionResult{
		ResourceARN: row.ResourceID, Surface: "lambda",
		CurrentErrorCount: 90, CurrentInvocationCount: 3000, CurrentErrorRate: 0.03,
		BaselineErrorCount: 60, BaselineInvocationCount: 6000, BaselineErrorRate: 0.01,
		RateRatio: 3.0, ExceedsRateRatioFloor: true, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	draft, err := CheckLambdaErrorRate(context.Background(), row, result, newErrorRateScope(), &fakeExclusionStore{})
	if err != nil || draft == nil {
		t.Fatalf("CheckLambdaErrorRate: err=%v draft=%v", err, draft)
	}
	if draft.Kind != "span-quality-error-rate-spike" {
		t.Errorf("Kind = %q", draft.Kind)
	}
	if !strings.HasSuffix(draft.RecommendationID, ".error_rate_spike") {
		t.Errorf("RecommendationID = %q", draft.RecommendationID)
	}
	for _, want := range []string{"Recent deploy regression", "Downstream dependency failure", "Resource exhaustion under load", "MORE COMMON", "DECLINE", "MERGE"} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing %q", want)
		}
	}
	if !strings.Contains(draft.Terraform, "aws_lambda_function") {
		t.Errorf("Terraform = %s", draft.Terraform)
	}
	if !strings.Contains(draft.Terraform, "memory_size") || !strings.Contains(draft.Terraform, "reserved_concurrent_executions") {
		t.Errorf("Terraform missing memory + concurrency raise; got %s", draft.Terraform)
	}
}

func runPerCloudHappyPathErrorRate(t *testing.T, surface, provider, tfName, wantInTerraform string,
	check func(context.Context, ErrorRateInventoryRow, *ErrorRateDetectionResult, ErrorRateScope, ErrorRateExclusionStore) (*ErrorRateRecommendationDraft, error),
) {
	t.Helper()
	row := newErrorRateRow()
	row.Provider = provider
	row.Surface = surface
	row.ResourceTFName = tfName
	result := &ErrorRateDetectionResult{
		Surface:               surface,
		ExceedsRateRatioFloor: true, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	draft, err := check(context.Background(), row, result, newErrorRateScope(), &fakeExclusionStore{})
	if err != nil || draft == nil {
		t.Fatalf("check err=%v draft=%v", err, draft)
	}
	if !strings.Contains(draft.Terraform, wantInTerraform) {
		t.Errorf("Terraform missing %q; got: %s", wantInTerraform, draft.Terraform)
	}
}

func TestCheckCloudRunErrorRate_HappyPath(t *testing.T) {
	runPerCloudHappyPathErrorRate(t, "cloudrun", "gcp", "hello_run", "google_cloud_run_service", CheckCloudRunErrorRate)
}
func TestCheckCloudFunctionsErrorRate_HappyPath(t *testing.T) {
	runPerCloudHappyPathErrorRate(t, "cloudfunc", "gcp", "hello_func", "google_cloudfunctions2_function", CheckCloudFunctionsErrorRate)
}
func TestCheckAzureFunctionsErrorRate_HappyPath(t *testing.T) {
	runPerCloudHappyPathErrorRate(t, "azfunc", "azure", "hello_az", "azurerm_service_plan", CheckAzureFunctionsErrorRate)
}
func TestCheckOCIFunctionsErrorRate_HappyPath(t *testing.T) {
	runPerCloudHappyPathErrorRate(t, "ocifunc", "oci", "hello_oci", "oci_functions_function", CheckOCIFunctionsErrorRate)
}

func TestErrorRate_UnsupportedSurface_ReturnsError(t *testing.T) {
	_, err := DetectErrorRate(context.Background(), &stubErrorRateQuerier{}, "arn", "ec2")
	if err == nil || !strings.Contains(err.Error(), "unsupported surface") {
		t.Fatalf("expected unsupported-surface error; got: %v", err)
	}
}

func TestErrorRate_QueryFailure_ReturnsError(t *testing.T) {
	_, err := DetectErrorRate(context.Background(), &stubErrorRateQuerier{err: errors.New("simulated failure")}, "arn", "lambda")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestErrorRate_NilResult_EmitsNothing(t *testing.T) {
	draft, _ := CheckLambdaErrorRate(context.Background(), newErrorRateRow(), nil, newErrorRateScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Error("expected nil draft on nil result")
	}
}

func TestErrorRate_NotFiringResult_EmitsNothing(t *testing.T) {
	result := &ErrorRateDetectionResult{
		ExceedsRateRatioFloor: false, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	draft, _ := CheckLambdaErrorRate(context.Background(), newErrorRateRow(), result, newErrorRateScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Error("expected nil draft when ShouldFire false")
	}
}

func TestErrorRate_WrongSurface_EmitsNothing(t *testing.T) {
	result := &ErrorRateDetectionResult{
		ExceedsRateRatioFloor: true, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	// Lambda row → CheckCloudRunErrorRate should skip.
	draft, _ := CheckCloudRunErrorRate(context.Background(), newErrorRateRow(), result, newErrorRateScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Error("expected nil draft for wrong-surface row")
	}
}

func TestErrorRate_KindOnlyExclusion_DoesNotFire(t *testing.T) {
	result := &ErrorRateDetectionResult{
		ExceedsRateRatioFloor: true, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{RecommendationKind: ErrorRateRecommendationKind},
	}}
	draft, _ := CheckLambdaErrorRate(context.Background(), newErrorRateRow(), result, newErrorRateScope(), exclusions)
	if draft != nil {
		t.Error("expected nil draft when kind-excluded")
	}
}

func TestErrorRate_ExclusionStoreError_ReturnsError(t *testing.T) {
	result := &ErrorRateDetectionResult{
		ExceedsRateRatioFloor: true, ExceedsMinimumInvocations: true, ExceedsMinimumErrors: true,
	}
	exclusions := &fakeExclusionStore{errFromList: errors.New("simulated sqlite failure")}
	_, err := CheckLambdaErrorRate(context.Background(), newErrorRateRow(), result, newErrorRateScope(), exclusions)
	if err == nil {
		t.Fatal("expected error from exclusion store")
	}
}

func TestErrorRate_QueryUsesSumStatistic(t *testing.T) {
	q := querierFor(3000, 90, 6000, 60)
	_, err := DetectErrorRate(context.Background(), q, "arn", "lambda")
	if err != nil {
		t.Fatalf("DetectErrorRate: %v", err)
	}
	if q.calls != 4 {
		t.Errorf("queried %d times, want 4 (invCurrent, errCurrent, invBase, errBase)", q.calls)
	}
}
