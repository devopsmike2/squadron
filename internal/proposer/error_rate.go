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
)

// error_rate.go — Error rate correlation slice 1 chunk 2
// (v0.89.128, #768 Stream 166). Sibling of sampling_rate.go and
// cold_start.go: pure detection branch the discovery proposer flow
// runs ALONGSIDE the existing serverless tier kinds. For each
// serverless inventory row whose detection result fires
// (ShouldFireRecommendation), the branch emits a
// span-quality-error-rate-spike draft.
//
// Reuses the chunk-1 substrate from v0.89.127: per-cloud
// MetricQuerier.QueryAggregate routes the new per-cloud error metric
// (LambdaErrorsMetricName etc.) alongside the invocation metric from
// sampling rate slice 1. The detection runs 4 metric queries per
// resource: current 24h invocations, current 24h errors, baseline
// 168h invocations, baseline 168h errors.
//
// See docs/proposals/error-rate-correlation-slice1.md §3 detection
// rule, §6.1 endpoint shape, §8 recommendation kind + 3-failure-mode
// reasoning + per-cloud Terraform patterns, §11 acceptance tests
// 6-9 + 12, §12 baseline-too-low guard.

// ErrorRateRatioFloor — the §3 step 6 ratio gate. Fires when
// current_error_rate > baseline_error_rate * 2.0. The cold-start arc
// used 1.5x; error rate gets the looser 2.0x because error metrics
// are noisier than latency metrics (see §3.1).
const ErrorRateRatioFloor = 2.0

// ErrorRateMinInvocationCount — the §3.2 statistical noise floor on
// the denominator. Below 1000 invocations in the 24h window the
// error percentage isn't operationally trustworthy.
const ErrorRateMinInvocationCount uint64 = 1000

// ErrorRateMinErrorCount — the §3.2 absolute floor on the
// numerator. A function with a baseline of 1 error/day showing 3
// today is 3x ratio but statistically meaningless. 50 errors in
// 24h corresponds to ~2/hour sustained — a real signal worth
// surfacing.
const ErrorRateMinErrorCount uint64 = 50

// ErrorRateBaselineFloor — the §12 baseline-too-low guard. When the
// baseline error rate would otherwise be essentially zero (no errors
// in the 168h window), comparing current > baseline * 2 would always
// fire even on a single error. The floor caps the comparison
// denominator at 0.0001 (0.01%) so a function that genuinely has a
// near-zero baseline doesn't get flagged on a handful of errors.
const ErrorRateBaselineFloor = 0.0001

// ErrorRateCurrentWindowHours — current observation window in
// hours. Mirrors sampling rate slice 1.
const ErrorRateCurrentWindowHours = 24

// ErrorRateBaselineWindowHours — baseline observation window in
// hours. Mirrors cold-start slice 1 (7 days).
const ErrorRateBaselineWindowHours = 168

// ErrorRateRecommendationKind — the §8 single recommendation kind
// the error-rate detection branch emits. Reuses the span-quality-
// webhook prefix from v0.89.86 so NO new webhook routing is
// needed.
const ErrorRateRecommendationKind = "span-quality-error-rate-spike"

// ErrorRateDetectionResult captures the comparison of current
// error rate vs baseline error rate per resource. Three booleans
// gate the fire condition; surfacing them independently lets the
// per-resource API endpoint render the partial state (e.g.
// "ratio fires but error count below absolute minimum — no fire").
type ErrorRateDetectionResult struct {
	ResourceARN             string
	Surface                 string
	CurrentErrorCount       uint64
	CurrentInvocationCount  uint64
	CurrentErrorRate        float64
	BaselineErrorCount      uint64
	BaselineInvocationCount uint64
	BaselineErrorRate       float64
	// BaselineAdjusted — §12 baseline-too-low guard fired:
	// BaselineErrorRate was below ErrorRateBaselineFloor and the
	// floor value was used as the comparison denominator. The
	// per-resource API endpoint surfaces this so operators know
	// the ratio is computed against a floor, not the raw baseline.
	BaselineAdjusted bool
	// RateRatio — CurrentErrorRate / (effective baseline). The
	// effective baseline is BaselineErrorRate when above the floor,
	// otherwise ErrorRateBaselineFloor.
	RateRatio                 float64
	ExceedsRateRatioFloor     bool // RateRatio > ErrorRateRatioFloor
	ExceedsMinimumInvocations bool // CurrentInvocationCount >= ErrorRateMinInvocationCount
	ExceedsMinimumErrors      bool // CurrentErrorCount >= ErrorRateMinErrorCount
	ObservedAt                time.Time
}

// ShouldFireRecommendation returns true when all three gates pass:
// the ratio exceeds 2.0x AND the denominator clears the noise floor
// AND the absolute error count clears the statistical-meaning floor.
// §3 step 6.
func (r ErrorRateDetectionResult) ShouldFireRecommendation() bool {
	return r.ExceedsRateRatioFloor && r.ExceedsMinimumInvocations && r.ExceedsMinimumErrors
}

// ErrorRateMetricQuerier — slim slice of scanner.MetricQuerier the
// detection branch needs. Distinct interface from
// SamplingRateMetricQuerier so test stubs don't collide even though
// the method shape is the same.
type ErrorRateMetricQuerier interface {
	QueryAggregate(
		ctx context.Context,
		resourceARN string,
		metricName string,
		window time.Duration,
		stat scanner.MetricStatistic,
	) (scanner.AggregateMetricResult, error)
}

// DetectErrorRate queries the cloud-native invocation count + error
// count over the 24h current window and the 168h baseline window
// for the given resource. Returns the comparison populated with
// the rate ratio + all three gate booleans.
//
// Four metric calls per resource per scan: current invocations,
// current errors, baseline invocations, baseline errors. Cost
// characteristics identical to cold-start slice 1+2 — the per-cloud
// rate limits absorb the volume comfortably per §12.
//
// Empty-result semantics: when an invocation count is 0, the
// derived rate is 0; the rate ratio is 0; ExceedsRateRatioFloor is
// false (0 > 2.0 is false), so ShouldFireRecommendation returns
// false. Same posture for absent-baseline cases except that the
// near-zero baseline guard at §12 kicks in to prevent a tiny
// non-zero baseline from producing a spurious large ratio.
func DetectErrorRate(
	ctx context.Context,
	querier ErrorRateMetricQuerier,
	resourceARN string,
	surface string,
) (ErrorRateDetectionResult, error) {
	invMetric, ok := samplingInvocationMetricFor(surface)
	if !ok {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: unsupported surface: %q", surface)
	}
	errMetric, ok := errorMetricFor(surface)
	if !ok {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: unsupported surface (error metric): %q", surface)
	}

	currentWindow := time.Duration(ErrorRateCurrentWindowHours) * time.Hour
	baselineWindow := time.Duration(ErrorRateBaselineWindowHours) * time.Hour

	currInv, err := querier.QueryAggregate(ctx, resourceARN, invMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current invocation count query: %w", err)
	}
	currErr, err := querier.QueryAggregate(ctx, resourceARN, errMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current error count query: %w", err)
	}
	baseInv, err := querier.QueryAggregate(ctx, resourceARN, invMetric, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline invocation count query: %w", err)
	}
	baseErr, err := querier.QueryAggregate(ctx, resourceARN, errMetric, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline error count query: %w", err)
	}

	result := ErrorRateDetectionResult{
		ResourceARN:             resourceARN,
		Surface:                 surface,
		CurrentInvocationCount:  posUint64(currInv.Value),
		CurrentErrorCount:       posUint64(currErr.Value),
		BaselineInvocationCount: posUint64(baseInv.Value),
		BaselineErrorCount:      posUint64(baseErr.Value),
		ObservedAt:              time.Now().UTC(),
	}
	FinalizeErrorRateGates(&result)
	return result, nil
}

// FinalizeErrorRateGates computes the derived error-rate fields — the current
// + baseline rates, the §12-adjusted rate ratio, and the three gate booleans —
// from the four raw counts already set on the result. Shared by DetectErrorRate
// and the recommendations-wiring layer (which reconstructs the result from
// persisted observation rows) so the gate thresholds live in exactly one place.
func FinalizeErrorRateGates(result *ErrorRateDetectionResult) {
	if result.CurrentInvocationCount > 0 {
		result.CurrentErrorRate = float64(result.CurrentErrorCount) / float64(result.CurrentInvocationCount)
	}
	if result.BaselineInvocationCount > 0 {
		result.BaselineErrorRate = float64(result.BaselineErrorCount) / float64(result.BaselineInvocationCount)
	}
	// §12 near-zero baseline guard. When the baseline rate would
	// otherwise be below the floor, the floor value becomes the
	// effective comparison denominator and the BaselineAdjusted
	// boolean surfaces the substitution to the operator.
	effectiveBaseline := result.BaselineErrorRate
	if effectiveBaseline < ErrorRateBaselineFloor {
		effectiveBaseline = ErrorRateBaselineFloor
		result.BaselineAdjusted = true
	}
	result.RateRatio = result.CurrentErrorRate / effectiveBaseline
	result.ExceedsRateRatioFloor = result.RateRatio > ErrorRateRatioFloor
	result.ExceedsMinimumInvocations = result.CurrentInvocationCount >= ErrorRateMinInvocationCount
	result.ExceedsMinimumErrors = result.CurrentErrorCount >= ErrorRateMinErrorCount
}

// posUint64 clamps a possibly-negative float metric value to a
// non-negative uint64. Cloud APIs sometimes return -0.0 or small
// negative values from sum-of-empty-window queries; treating those
// as 0 keeps the detection arithmetic well-defined.
func posUint64(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}

// errorMetricFor returns the per-cloud error count metric name per
// §4. Slice-1 chunk-1 extended each cloud's QueryAggregate to route
// these constants alongside the existing invocation count constants.
func errorMetricFor(surface string) (string, bool) {
	switch surface {
	case "lambda":
		return aws.LambdaErrorsMetricName, true
	case "cloudrun":
		return gcp.CloudRunRequestCount5xxMetricType, true
	case "cloudfunc":
		return gcp.CloudFunctionsExecutionCountErrorMetricType, true
	case "azfunc":
		return azure.AzureFunctionsErrorsMetric, true
	case "ocifunc":
		return oci.OCIFunctionsErrorResponseCountMetric, true
	}
	return "", false
}

// ErrorRateInventoryRow is the per-row projection the error-rate
// detection branch reads. Mirrors SamplingRateInventoryRow.
type ErrorRateInventoryRow struct {
	RecommendationID string
	Provider         string // "aws" / "gcp" / "azure" / "oci"
	Surface          string // "lambda" / "cloudrun" / "cloudfunc" / "azfunc" / "ocifunc"
	ResourceTFName   string
	ResourceID       string
	Region           string
}

// ErrorRateScope mirrors SamplingRateScope.
type ErrorRateScope struct {
	ConnectionID string
	ScopeID      string
	Region       string
}

// ErrorRateRecommendationDraft is the per-row output of the
// error-rate detection branch — projected at the handler boundary
// into recommendations.Recommendation.
type ErrorRateRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
}

// ErrorRateExclusionStore — slim slice of ApplicationStore the
// detection branch consults. Satisfied by
// applicationstore.ApplicationStore.
type ErrorRateExclusionStore interface {
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// CheckLambdaErrorRate is the §8 AWS Lambda detection branch.
// Returns nil draft when: result is nil OR ShouldFire is false, the
// row has been excluded by the operator, or the surface isn't
// "lambda".
func CheckLambdaErrorRate(
	ctx context.Context,
	row ErrorRateInventoryRow,
	result *ErrorRateDetectionResult,
	scope ErrorRateScope,
	exclusions ErrorRateExclusionStore,
) (*ErrorRateRecommendationDraft, error) {
	return checkErrorRateGeneric(ctx, row, result, scope, exclusions, "lambda", "aws")
}

// CheckCloudRunErrorRate — sibling for GCP Cloud Run.
func CheckCloudRunErrorRate(
	ctx context.Context, row ErrorRateInventoryRow, result *ErrorRateDetectionResult,
	scope ErrorRateScope, exclusions ErrorRateExclusionStore,
) (*ErrorRateRecommendationDraft, error) {
	return checkErrorRateGeneric(ctx, row, result, scope, exclusions, "cloudrun", "gcp")
}

// CheckCloudFunctionsErrorRate — sibling for GCP Cloud Functions.
func CheckCloudFunctionsErrorRate(
	ctx context.Context, row ErrorRateInventoryRow, result *ErrorRateDetectionResult,
	scope ErrorRateScope, exclusions ErrorRateExclusionStore,
) (*ErrorRateRecommendationDraft, error) {
	return checkErrorRateGeneric(ctx, row, result, scope, exclusions, "cloudfunc", "gcp")
}

// CheckAzureFunctionsErrorRate — sibling for Azure Functions.
func CheckAzureFunctionsErrorRate(
	ctx context.Context, row ErrorRateInventoryRow, result *ErrorRateDetectionResult,
	scope ErrorRateScope, exclusions ErrorRateExclusionStore,
) (*ErrorRateRecommendationDraft, error) {
	return checkErrorRateGeneric(ctx, row, result, scope, exclusions, "azfunc", "azure")
}

// CheckOCIFunctionsErrorRate — sibling for OCI Functions.
func CheckOCIFunctionsErrorRate(
	ctx context.Context, row ErrorRateInventoryRow, result *ErrorRateDetectionResult,
	scope ErrorRateScope, exclusions ErrorRateExclusionStore,
) (*ErrorRateRecommendationDraft, error) {
	return checkErrorRateGeneric(ctx, row, result, scope, exclusions, "ocifunc", "oci")
}

// checkErrorRateGeneric is the shared body all 5 per-cloud helpers
// delegate to. The exclusion check uses the ".error_rate_spike"
// suffix convention so operators can exclude the error-rate kind
// without excluding cold-start / sampling-rate / serverless-tier
// kinds that share the same recommendation root.
func checkErrorRateGeneric(
	ctx context.Context,
	row ErrorRateInventoryRow,
	result *ErrorRateDetectionResult,
	scope ErrorRateScope,
	exclusions ErrorRateExclusionStore,
	expectedSurface, provider string,
) (*ErrorRateRecommendationDraft, error) {
	if result == nil || !result.ShouldFireRecommendation() {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != expectedSurface {
		return nil, nil
	}
	recID := row.RecommendationID + ".error_rate_spike"
	if exclusions != nil && scope.ConnectionID != "" && scope.ScopeID != "" {
		excluded, err := exclusions.ListExcludedRecommendations(
			ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
		)
		if err != nil {
			return nil, fmt.Errorf("error rate: list excluded recommendations: %w", err)
		}
		for _, ex := range excluded {
			if ex.RecommendationID != "" && ex.RecommendationID == recID {
				return nil, nil
			}
			if ex.RecommendationID == "" && ex.RecommendationKind == ErrorRateRecommendationKind {
				return nil, nil
			}
		}
	}
	return &ErrorRateRecommendationDraft{
		Kind:             ErrorRateRecommendationKind,
		RecommendationID: recID,
		Reasoning:        formatErrorRateReasoning(row, result),
		Terraform:        iacpicker.PickErrorRateTerraform(provider, expectedSurface, row.ResourceTFName),
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
	}, nil
}

// formatErrorRateReasoning composes the operator-facing reasoning
// for the span-quality-error-rate-spike kind. The §8 three-failure-
// mode framing explicitly notes that cases (1) and (2) — recent
// deploy regression and downstream dependency failure — are the
// MORE COMMON causes and should be declined; the Terraform PR
// targets only case (3), resource exhaustion under load.
func formatErrorRateReasoning(row ErrorRateInventoryRow, result *ErrorRateDetectionResult) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This %s emitted %d errors across %d invocations over the last 24 hours (current error rate: %.2f%%).\n\n",
		humanSurface(row.Surface), result.CurrentErrorCount, result.CurrentInvocationCount, result.CurrentErrorRate*100,
	)
	fmt.Fprintf(&b,
		"7-day baseline: %d errors across %d invocations (baseline error rate: %.2f%%). Current/baseline ratio: %.2fx.\n",
		result.BaselineErrorCount, result.BaselineInvocationCount, result.BaselineErrorRate*100, result.RateRatio,
	)
	if result.BaselineAdjusted {
		fmt.Fprintf(&b,
			"Note: the raw baseline rate was below the %.4f%% near-zero floor, so the ratio above was computed against the floor as the comparison denominator (not the raw baseline). See §12 of the design doc.\n",
			ErrorRateBaselineFloor*100,
		)
	}
	fmt.Fprintf(&b, "\nResource: %s (provider=%s, region=%s).\n\n", row.ResourceID, row.Provider, row.Region)
	b.WriteString("Squadron flags this when the current/baseline ratio exceeds 2.0x AND the resource processed at least 1000 invocations AND at least 50 errors in the window. Three common causes — pick the one matching your deployment:\n")
	b.WriteString("  1. Recent deploy regression — MORE COMMON. Check the function's deployment timeline. If errors started after a deploy, revert or fix the regression at the application layer. This Terraform PR does NOT fix application bugs. DECLINE if your cause is (1).\n")
	b.WriteString("  2. Downstream dependency failure — MORE COMMON. If the function calls a database / API / queue that's failing, errors propagate. Investigate the downstream first. DECLINE if your cause is (2).\n")
	b.WriteString("  3. Resource exhaustion under load. Throttling, memory pressure, connection pool exhaustion. This Terraform PR raises memory + concurrency limits to give the function headroom. MERGE if your cause is (3).\n\n")
	b.WriteString("Cases (1) and (2) are the more common causes — the verdict learning loop records declines so Squadron's future runs learn the distribution for your fleet.\n")
	return b.String()
}
