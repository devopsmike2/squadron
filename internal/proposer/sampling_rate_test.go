// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// sampling_rate_test.go — Sampling rate analysis slice 1 chunk 2
// (v0.89.123, #763 Stream 161). Pins the §3 detection rule + §11
// acceptance tests 8-13 + the per-cloud Check helpers.

type stubSamplingQuerier struct {
	value     float64
	err       error
	gotMetric string
	gotWindow time.Duration
	gotStat   scanner.MetricStatistic
}

func (s *stubSamplingQuerier) QueryAggregate(
	_ context.Context, resourceARN, metricName string, window time.Duration, stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	s.gotMetric, s.gotWindow, s.gotStat = metricName, window, stat
	if s.err != nil {
		return scanner.AggregateMetricResult{}, s.err
	}
	return scanner.AggregateMetricResult{
		ResourceARN: resourceARN, MetricName: metricName, Window: window,
		Statistic: stat, Value: s.value, ObservedAt: time.Now().UTC(),
	}, nil
}

type stubSpanCounter struct {
	count uint64
	ok    bool
}

func (s *stubSpanCounter) SpanCountLast24h(_ string) (uint64, bool) { return s.count, s.ok }

func newSamplingRow() SamplingRateInventoryRow {
	return SamplingRateInventoryRow{
		RecommendationID: "rec-aws-lambda-order-processor",
		Provider:         "aws", Surface: "lambda",
		ResourceID:     "arn:aws:lambda:us-east-1:123:function:order-processor",
		ResourceTFName: "order_processor", Region: "us-east-1",
	}
}

func newSamplingScope() SamplingRateScope {
	return SamplingRateScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}
}

// Test 8 — ratio 4.9% at 5000 invocations FIRES.
func TestSamplingRate_RatioBelow5PctAt5000Invocations_FiresRecommendation(t *testing.T) {
	res, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{value: 5000}, &stubSpanCounter{count: 245, ok: true},
		"arn", "lambda", "k")
	if err != nil {
		t.Fatalf("DetectSamplingRate: %v", err)
	}
	if res.Ratio >= SamplingRatioFloor {
		t.Errorf("Ratio = %v, want < %v", res.Ratio, SamplingRatioFloor)
	}
	if !res.ExceedsFloor || !res.ExceedsMinimumInvocations || !res.ShouldFireRecommendation() {
		t.Errorf("predicates not all true; got ExceedsFloor=%v ExceedsMin=%v ShouldFire=%v",
			res.ExceedsFloor, res.ExceedsMinimumInvocations, res.ShouldFireRecommendation())
	}
}

// Test 9 — ratio == 5.0% does NOT fire (strict-less-than rule).
func TestSamplingRate_RatioAt5PctAt5000Invocations_DoesNotFire(t *testing.T) {
	res, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{value: 5000}, &stubSpanCounter{count: 250, ok: true},
		"arn", "lambda", "k")
	if err != nil {
		t.Fatalf("DetectSamplingRate: %v", err)
	}
	if res.Ratio != 0.05 {
		t.Errorf("Ratio = %v, want exactly 0.05", res.Ratio)
	}
	if res.ExceedsFloor || res.ShouldFireRecommendation() {
		t.Error("ShouldFire/ExceedsFloor true at exactly 5%, want false (strict-less-than)")
	}
}

// Test 10 — below noise floor (500 invocations < 1000 minimum).
func TestSamplingRate_RatioBelow5PctAt500Invocations_DoesNotFire(t *testing.T) {
	res, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{value: 500}, &stubSpanCounter{count: 10, ok: true},
		"arn", "lambda", "k")
	if err != nil {
		t.Fatalf("DetectSamplingRate: %v", err)
	}
	if !res.ExceedsFloor {
		t.Error("ExceedsFloor = false at 2%, want true")
	}
	if res.ExceedsMinimumInvocations || res.ShouldFireRecommendation() {
		t.Error("ShouldFire/ExceedsMin true at 500 invocations, want false")
	}
}

// Test 11 — null traceindex count + null invocations = no fire.
func TestSamplingRate_NullSpanCount_DoesNotFire(t *testing.T) {
	res, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{value: 0}, &stubSpanCounter{count: 0, ok: false},
		"arn", "lambda", "k")
	if err != nil {
		t.Fatalf("DetectSamplingRate: %v", err)
	}
	if res.ShouldFireRecommendation() {
		t.Error("ShouldFire = true at null/null, want false")
	}
}

// Zero spans w/ real invocations IS aggressive sampling — fires.
func TestSamplingRate_ZeroSpansWithEnoughInvocations_Fires(t *testing.T) {
	res, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{value: 3000}, &stubSpanCounter{count: 0, ok: true},
		"arn", "lambda", "k")
	if err != nil {
		t.Fatalf("DetectSamplingRate: %v", err)
	}
	if !res.ShouldFireRecommendation() {
		t.Error("ShouldFire = false at 0/3000, want true")
	}
}

func TestSamplingRate_QueryFailure_ReturnsError(t *testing.T) {
	_, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{err: errors.New("simulated failure")}, &stubSpanCounter{}, "arn", "lambda", "k")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestSamplingRate_UnsupportedSurface_ReturnsError(t *testing.T) {
	_, err := DetectSamplingRate(context.Background(),
		&stubSamplingQuerier{}, &stubSpanCounter{}, "arn", "ec2", "k")
	if err == nil || !strings.Contains(err.Error(), "unsupported surface") {
		t.Fatalf("expected unsupported-surface error; got: %v", err)
	}
}

func TestSamplingInvocationMetricFor_AllFiveSurfaces(t *testing.T) {
	cases := map[string]string{
		"lambda":    "Invocations",
		"cloudrun":  "run.googleapis.com/request_count",
		"cloudfunc": "cloudfunctions.googleapis.com/function/execution_count",
		"azfunc":    "FunctionInvocations",
		"ocifunc":   "function_invocation_count",
	}
	for surface, want := range cases {
		got, ok := samplingInvocationMetricFor(surface)
		if !ok || got != want {
			t.Errorf("samplingInvocationMetricFor(%q) = (%q, %v), want (%q, true)", surface, got, ok, want)
		}
	}
	if _, ok := samplingInvocationMetricFor("unknown"); ok {
		t.Error("unknown surface should be unsupported")
	}
}

func TestSamplingRate_QueryUsesSumStatisticAnd24hWindow(t *testing.T) {
	q := &stubSamplingQuerier{value: 100}
	_, err := DetectSamplingRate(context.Background(), q, &stubSpanCounter{count: 1, ok: true}, "arn", "lambda", "k")
	if err != nil {
		t.Fatalf("DetectSamplingRate: %v", err)
	}
	if q.gotStat != scanner.StatisticSum || q.gotWindow != 24*time.Hour || q.gotMetric != "Invocations" {
		t.Errorf("got (stat=%v window=%v metric=%v), want (sum 24h Invocations)", q.gotStat, q.gotWindow, q.gotMetric)
	}
}

func TestCheckLambdaSamplingRate_HappyPath(t *testing.T) {
	row := newSamplingRow()
	result := &SamplingRateDetectionResult{
		ResourceARN: row.ResourceID, Surface: "lambda",
		ObservedSpanCount: 245, ExpectedInvocationCount: 5000, Ratio: 0.049,
		ExceedsFloor: true, ExceedsMinimumInvocations: true,
	}
	draft, err := CheckLambdaSamplingRate(context.Background(), row, result, newSamplingScope(), &fakeExclusionStore{})
	if err != nil || draft == nil {
		t.Fatalf("CheckLambdaSamplingRate: err=%v draft=%v", err, draft)
	}
	if draft.Kind != "span-quality-sampling-too-aggressive" {
		t.Errorf("Kind = %q", draft.Kind)
	}
	if !strings.HasSuffix(draft.RecommendationID, ".sampling_too_aggressive") {
		t.Errorf("RecommendationID = %q", draft.RecommendationID)
	}
	for _, want := range []string{"Default sampler too aggressive", "Adaptive sampling throttling", "Tail-sampling collector"} {
		if !strings.Contains(draft.Reasoning, want) {
			t.Errorf("Reasoning missing %q", want)
		}
	}
	if !strings.Contains(draft.Terraform, "OTEL_TRACES_SAMPLER_ARG") || !strings.Contains(draft.Terraform, "aws_lambda_function") {
		t.Errorf("Terraform = %s", draft.Terraform)
	}
}

func runPerCloudHappyPath(t *testing.T, surface, provider, tfName, wantInTerraform string,
	check func(context.Context, SamplingRateInventoryRow, *SamplingRateDetectionResult, SamplingRateScope, SamplingRateExclusionStore) (*SamplingRateRecommendationDraft, error),
) {
	t.Helper()
	row := newSamplingRow()
	row.Provider = provider
	row.Surface = surface
	row.ResourceTFName = tfName
	result := &SamplingRateDetectionResult{Surface: surface, ExceedsFloor: true, ExceedsMinimumInvocations: true}
	draft, err := check(context.Background(), row, result, newSamplingScope(), &fakeExclusionStore{})
	if err != nil || draft == nil {
		t.Fatalf("check err=%v draft=%v", err, draft)
	}
	if !strings.Contains(draft.Terraform, wantInTerraform) {
		t.Errorf("Terraform missing %q; got: %s", wantInTerraform, draft.Terraform)
	}
}

func TestCheckCloudRunSamplingRate_HappyPath(t *testing.T) {
	runPerCloudHappyPath(t, "cloudrun", "gcp", "hello_run", "google_cloud_run_service", CheckCloudRunSamplingRate)
}
func TestCheckCloudFunctionsSamplingRate_HappyPath(t *testing.T) {
	runPerCloudHappyPath(t, "cloudfunc", "gcp", "hello_func", "google_cloudfunctions2_function", CheckCloudFunctionsSamplingRate)
}
func TestCheckAzureFunctionsSamplingRate_HappyPath(t *testing.T) {
	runPerCloudHappyPath(t, "azfunc", "azure", "hello_az", "azurerm_linux_function_app", CheckAzureFunctionsSamplingRate)
}
func TestCheckOCIFunctionsSamplingRate_HappyPath(t *testing.T) {
	runPerCloudHappyPath(t, "ocifunc", "oci", "hello_oci", "oci_functions_function", CheckOCIFunctionsSamplingRate)
}

func TestSamplingRate_NotFiringResult_EmitsNothing(t *testing.T) {
	result := &SamplingRateDetectionResult{ExceedsFloor: false, ExceedsMinimumInvocations: true}
	draft, _ := CheckLambdaSamplingRate(context.Background(), newSamplingRow(), result, newSamplingScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Error("expected nil draft when ShouldFire false")
	}
}

func TestSamplingRate_NilResult_EmitsNothing(t *testing.T) {
	draft, _ := CheckLambdaSamplingRate(context.Background(), newSamplingRow(), nil, newSamplingScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Error("expected nil draft on nil result")
	}
}

func TestSamplingRate_WrongSurface_EmitsNothing(t *testing.T) {
	result := &SamplingRateDetectionResult{ExceedsFloor: true, ExceedsMinimumInvocations: true}
	// Lambda row → CheckCloudRunSamplingRate should skip.
	draft, _ := CheckCloudRunSamplingRate(context.Background(), newSamplingRow(), result, newSamplingScope(), &fakeExclusionStore{})
	if draft != nil {
		t.Error("expected nil draft for wrong-surface row")
	}
}

func TestSamplingRate_ExcludedRow_DoesNotFire(t *testing.T) {
	row := newSamplingRow()
	result := &SamplingRateDetectionResult{ExceedsFloor: true, ExceedsMinimumInvocations: true}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{RecommendationID: row.RecommendationID + ".sampling_too_aggressive"},
	}}
	draft, _ := CheckLambdaSamplingRate(context.Background(), row, result, newSamplingScope(), exclusions)
	if draft != nil {
		t.Error("expected nil draft for excluded row")
	}
}

func TestSamplingRate_KindOnlyExclusion_DoesNotFire(t *testing.T) {
	result := &SamplingRateDetectionResult{ExceedsFloor: true, ExceedsMinimumInvocations: true}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{RecommendationKind: SamplingRateRecommendationKind},
	}}
	draft, _ := CheckLambdaSamplingRate(context.Background(), newSamplingRow(), result, newSamplingScope(), exclusions)
	if draft != nil {
		t.Error("expected nil draft when kind-excluded")
	}
}

func TestSamplingRate_ExclusionStoreError_ReturnsError(t *testing.T) {
	result := &SamplingRateDetectionResult{ExceedsFloor: true, ExceedsMinimumInvocations: true}
	exclusions := &fakeExclusionStore{errFromList: errors.New("simulated sqlite failure")}
	_, err := CheckLambdaSamplingRate(context.Background(), newSamplingRow(), result, newSamplingScope(), exclusions)
	if err == nil {
		t.Fatal("expected error from exclusion store")
	}
}
