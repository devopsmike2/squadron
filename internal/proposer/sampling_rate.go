// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/aws"
	"github.com/devopsmike2/squadron/internal/discovery/azure"
	"github.com/devopsmike2/squadron/internal/discovery/gcp"
	"github.com/devopsmike2/squadron/internal/discovery/oci"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/proposer/iacpicker"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// sampling_rate.go — Sampling rate analysis slice 1 chunk 2
// (v0.89.123, #763 Stream 161). Sibling of cold_start.go: pure
// detection branch the discovery proposer flow runs ALONGSIDE the
// existing serverless tier kinds. For each serverless inventory row
// whose detection result fires (ShouldFireRecommendation), the
// branch emits a span-quality-sampling-too-aggressive draft.
//
// Reuses the chunk-1 substrate from v0.89.122: traceindex.Quality.
// SpanCountLast24h(key) for observed_span_count, per-cloud
// MetricQuerier.QueryAggregate(invocation_metric) for
// expected_invocation_count.
//
// See docs/proposals/sampling-rate-analysis-slice1.md §3 detection
// rule, §6.1 endpoint shape, §8 recommendation kind + 3-failure-mode
// reasoning + per-cloud Terraform patterns, §11 acceptance tests
// 8-13.

// SamplingRatioFloor — the §3 5% floor below which a sustained
// observed/expected ratio counts as "too aggressive". STRICT-LESS-
// THAN: ratio < 0.05 fires; ratio == 0.05 does NOT (acceptance
// test 9 pins this).
const SamplingRatioFloor = 0.05

// MinInvocationCount — the §3.2 statistical noise floor. Below 1000
// invocations in the 24h window the percentages aren't trustworthy.
const MinInvocationCount uint64 = 1000

// SamplingRateRecommendationKind — the §8 single recommendation kind
// the sampling-rate detection branch emits. Reuses the span-quality-
// webhook prefix from v0.89.86 so NO new webhook routing is needed.
const SamplingRateRecommendationKind = "span-quality-sampling-too-aggressive"

// SamplingRateDetectionResult captures the comparison of observed
// span count vs expected invocation count per resource over the 24h
// window. The two booleans surface independently on the API
// response so operators can distinguish "below floor but above
// minimum" (would fire) from "below floor AND below minimum"
// (would NOT fire; statistical noise filter).
type SamplingRateDetectionResult struct {
	ResourceARN               string
	Surface                   string
	ObservedSpanCount         uint64
	ExpectedInvocationCount   uint64
	Ratio                     float64
	ExceedsFloor              bool // Ratio < SamplingRatioFloor
	ExceedsMinimumInvocations bool // ExpectedInvocationCount >= MinInvocationCount
	ObservedAt                time.Time
}

// ShouldFireRecommendation returns true when the §3 step 4
// predicate holds: ratio < floor AND invocation_count >= minimum.
// Both gates must be satisfied — the at-floor case and the
// below-minimum case both decline.
func (r SamplingRateDetectionResult) ShouldFireRecommendation() bool {
	return r.ExceedsFloor && r.ExceedsMinimumInvocations
}

// SamplingRateMetricQuerier — slim slice of scanner.MetricQuerier
// the detection branch needs. Lifted so tests don't pull each
// cloud's scanner implementation.
type SamplingRateMetricQuerier interface {
	QueryAggregate(
		ctx context.Context,
		resourceARN string,
		metricName string,
		window time.Duration,
		stat scanner.MetricStatistic,
	) (scanner.AggregateMetricResult, error)
}

// SamplingRateSpanCounter — slim slice of *traceindex.Quality the
// detection branch needs. Production wires the real
// *traceindex.Quality (compile-time check below).
type SamplingRateSpanCounter interface {
	SpanCountLast24h(key string) (uint64, bool)
}

var _ SamplingRateSpanCounter = (*traceindex.Quality)(nil)

// DetectSamplingRate queries the cloud-native invocation count and
// Squadron's traceindex 24h span count for the given resource.
// Returns the comparison populated with ratio + both predicates.
//
// Empty-result semantics: when invocation count is 0, Ratio is 0,
// ExceedsFloor is true (0 < 0.05), ExceedsMinimumInvocations is
// false (0 < 1000), so ShouldFireRecommendation returns false.
//
// Missing traceindex data: SpanCountLast24h ok=false leaves
// ObservedSpanCount at 0 — same posture as observed-zero-spans
// (acceptance test 11).
func DetectSamplingRate(
	ctx context.Context,
	querier SamplingRateMetricQuerier,
	qual SamplingRateSpanCounter,
	resourceARN string,
	surface string,
	traceindexKey string,
) (SamplingRateDetectionResult, error) {
	metricName, ok := samplingInvocationMetricFor(surface)
	if !ok {
		return SamplingRateDetectionResult{}, fmt.Errorf("sampling rate: unsupported surface: %q", surface)
	}
	inv, err := querier.QueryAggregate(ctx, resourceARN, metricName, 24*time.Hour, scanner.StatisticSum)
	if err != nil {
		return SamplingRateDetectionResult{}, fmt.Errorf("sampling rate: invocation count query: %w", err)
	}
	var spans uint64
	if qual != nil {
		spans, _ = qual.SpanCountLast24h(traceindexKey)
	}
	var invocations uint64
	if inv.Value > 0 {
		invocations = uint64(inv.Value)
	}
	result := SamplingRateDetectionResult{
		ResourceARN:             resourceARN,
		Surface:                 surface,
		ObservedSpanCount:       spans,
		ExpectedInvocationCount: invocations,
		ObservedAt:              time.Now().UTC(),
	}
	if invocations > 0 {
		result.Ratio = float64(spans) / float64(invocations)
	}
	result.ExceedsFloor = result.Ratio < SamplingRatioFloor
	result.ExceedsMinimumInvocations = result.ExpectedInvocationCount >= MinInvocationCount
	return result, nil
}

// samplingInvocationMetricFor returns the per-cloud invocation
// count metric name per §4. Each per-cloud chunk-1 (v0.89.122)
// extension already routes the metric through QueryAggregate.
func samplingInvocationMetricFor(surface string) (string, bool) {
	switch surface {
	case "lambda":
		return aws.LambdaInvocationsMetricName, true
	case "cloudrun":
		return gcp.CloudRunRequestCountMetricType, true
	case "cloudfunc":
		return gcp.CloudFunctionsExecutionCountMetricType, true
	case "azfunc":
		return azure.AzureFunctionsInvocationsMetric, true
	case "ocifunc":
		return oci.OCIFunctionsInvocationCountMetric, true
	}
	return "", false
}

// SamplingRateInventoryRow is the per-row projection the
// sampling-rate detection branch reads. Same posture as
// ColdStartInventoryRow — stated as a struct here so the proposer
// package stays disjoint from each cloud's scanner.
type SamplingRateInventoryRow struct {
	RecommendationID string
	Provider         string // "aws" / "gcp" / "azure" / "oci"
	Surface          string // "lambda" / "cloudrun" / "cloudfunc" / "azfunc" / "ocifunc"
	ResourceTFName   string
	ResourceID       string
	Region           string
}

// SamplingRateScope mirrors ColdStartScope.
type SamplingRateScope struct {
	ConnectionID string
	ScopeID      string
	Region       string
}

// SamplingRateRecommendationDraft is the per-row output of the
// sampling-rate detection branch — projected at the handler
// boundary into recommendations.Recommendation.
type SamplingRateRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
}

// SamplingRateExclusionStore — slim slice of ApplicationStore the
// detection branch consults. Satisfied by
// applicationstore.ApplicationStore.
type SamplingRateExclusionStore interface {
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// CheckLambdaSamplingRate is the §8 AWS Lambda detection branch.
// Returns nil draft when: result is nil OR ShouldFire is false, the
// row has been excluded by the operator, or the surface isn't
// "lambda".
func CheckLambdaSamplingRate(
	ctx context.Context,
	row SamplingRateInventoryRow,
	result *SamplingRateDetectionResult,
	scope SamplingRateScope,
	exclusions SamplingRateExclusionStore,
) (*SamplingRateRecommendationDraft, error) {
	return checkSamplingRateGeneric(ctx, row, result, scope, exclusions, "lambda", "aws")
}

// CheckCloudRunSamplingRate — sibling for GCP Cloud Run.
func CheckCloudRunSamplingRate(
	ctx context.Context, row SamplingRateInventoryRow, result *SamplingRateDetectionResult,
	scope SamplingRateScope, exclusions SamplingRateExclusionStore,
) (*SamplingRateRecommendationDraft, error) {
	return checkSamplingRateGeneric(ctx, row, result, scope, exclusions, "cloudrun", "gcp")
}

// CheckCloudFunctionsSamplingRate — sibling for GCP Cloud Functions.
func CheckCloudFunctionsSamplingRate(
	ctx context.Context, row SamplingRateInventoryRow, result *SamplingRateDetectionResult,
	scope SamplingRateScope, exclusions SamplingRateExclusionStore,
) (*SamplingRateRecommendationDraft, error) {
	return checkSamplingRateGeneric(ctx, row, result, scope, exclusions, "cloudfunc", "gcp")
}

// CheckAzureFunctionsSamplingRate — sibling for Azure Functions.
func CheckAzureFunctionsSamplingRate(
	ctx context.Context, row SamplingRateInventoryRow, result *SamplingRateDetectionResult,
	scope SamplingRateScope, exclusions SamplingRateExclusionStore,
) (*SamplingRateRecommendationDraft, error) {
	return checkSamplingRateGeneric(ctx, row, result, scope, exclusions, "azfunc", "azure")
}

// CheckOCIFunctionsSamplingRate — sibling for OCI Functions.
func CheckOCIFunctionsSamplingRate(
	ctx context.Context, row SamplingRateInventoryRow, result *SamplingRateDetectionResult,
	scope SamplingRateScope, exclusions SamplingRateExclusionStore,
) (*SamplingRateRecommendationDraft, error) {
	return checkSamplingRateGeneric(ctx, row, result, scope, exclusions, "ocifunc", "oci")
}

// checkSamplingRateGeneric is the shared body all 5 per-cloud
// helpers delegate to. The exclusion check uses the
// ".sampling_too_aggressive" suffix convention so operators can
// exclude the sampling kind without excluding cold-start /
// serverless-tier kinds that share the same recommendation root.
func checkSamplingRateGeneric(
	ctx context.Context,
	row SamplingRateInventoryRow,
	result *SamplingRateDetectionResult,
	scope SamplingRateScope,
	exclusions SamplingRateExclusionStore,
	expectedSurface, provider string,
) (*SamplingRateRecommendationDraft, error) {
	if result == nil || !result.ShouldFireRecommendation() {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != expectedSurface {
		return nil, nil
	}
	recID := row.RecommendationID + ".sampling_too_aggressive"
	if exclusions != nil && scope.ConnectionID != "" && scope.ScopeID != "" {
		excluded, err := exclusions.ListExcludedRecommendations(
			ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
		)
		if err != nil {
			return nil, fmt.Errorf("sampling rate: list excluded recommendations: %w", err)
		}
		for _, ex := range excluded {
			if ex.RecommendationID != "" && ex.RecommendationID == recID {
				return nil, nil
			}
			if ex.RecommendationID == "" && ex.RecommendationKind == SamplingRateRecommendationKind {
				return nil, nil
			}
		}
	}
	return &SamplingRateRecommendationDraft{
		Kind:             SamplingRateRecommendationKind,
		RecommendationID: recID,
		Reasoning:        formatSamplingReasoning(row, result),
		Terraform:        iacpicker.PickSamplingRateTerraform(provider, expectedSurface, row.ResourceTFName),
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
	}, nil
}

// formatSamplingReasoning composes the operator-facing reasoning
// for the span-quality-sampling-too-aggressive kind. Uniform
// 3-failure-mode framing across all 5 surfaces per §8.
func formatSamplingReasoning(row SamplingRateInventoryRow, result *SamplingRateDetectionResult) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This %s emitted %d spans over the last 24 hours against %d expected invocations (ratio: %.2f%%).\n\n",
		humanSurface(row.Surface), result.ObservedSpanCount, result.ExpectedInvocationCount, result.Ratio*100,
	)
	fmt.Fprintf(&b, "Resource: %s (provider=%s, region=%s).\n\n", row.ResourceID, row.Provider, row.Region)
	b.WriteString("Squadron flags this when the ratio is below 5% AND the resource processed at least 1000 invocations in the window. Three common causes — pick the one matching your deployment:\n")
	b.WriteString("  1. Default sampler too aggressive: many OTel SDKs default to TRACEIDRATIO_BASED at 0.1 (10%), but some framework integrations default lower. Check the SDK configuration.\n")
	b.WriteString("  2. Adaptive sampling throttling: Application Insights and some OTel exporters use adaptive sampling that throttles under load. The ratio Squadron sees is the OPERATOR-EXPERIENCED rate, not the configured rate. Decline if intentional.\n")
	b.WriteString("  3. Tail-sampling collector: a tail-sampling collector in front of Squadron's OTLP receiver selectively keeps spans. If that's intentional, decline — the verdict learning loop records.\n\n")
	b.WriteString("This Terraform PR raises OTEL_TRACES_SAMPLER_ARG to 0.5 (50%) as a starting point; operators tune from there. If your case is (2) or (3), decline the PR.\n")
	return b.String()
}

func humanSurface(surface string) string {
	switch surface {
	case "lambda":
		return "Lambda function"
	case "cloudrun":
		return "Cloud Run service"
	case "cloudfunc":
		return "Cloud Function"
	case "azfunc":
		return "Azure Function"
	case "ocifunc":
		return "OCI Function"
	}
	return "serverless resource"
}
