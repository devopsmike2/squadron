// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"
	"strings"

	"github.com/devopsmike2/squadron/internal/proposer/iacpicker"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// cold_start.go — Cold-start latency analysis slice 1 chunk 3
// (v0.89.115, #753 Stream 151). Sibling of trace_emission.go and
// span_quality.go: pure detection branch the discovery proposer flow
// runs ALONGSIDE the existing serverless tier kinds. For each Lambda
// inventory row whose chunk-2 cold-start detection result fires
// (ShouldFireRecommendation), the branch emits a
// lambda-cold-start-baseline draft.
//
// The branch does NOT re-run the CloudWatch query — that work happens
// in chunk 2 inside the scanner (runColdStartDetectionForServerless +
// DetectColdStartRegression). The caller threads a pre-computed
// ColdStartDetectionFinding per row sourced from the latest
// cold_start_observation entries persisted at scan time.
//
// See docs/proposals/cold-start-latency-slice1.md §8 + §11 acceptance
// tests 12-14.

// ColdStartInventoryRow is the per-row projection the cold-start
// detection branch reads. The wiring layer projects it from the AWS
// serverless inventory + the chunk-2 detection result at the API
// handler boundary so this package doesn't import the scanner / the
// internal/discovery/aws cold_start type.
//
// Slice 1 ships AWS Lambda only; the Provider + Surface fields are
// future-proofing for the slice 2 GCP / Azure / OCI cold-start kinds
// that re-use the same draft shape.
type ColdStartInventoryRow struct {
	// RecommendationID is the deterministic identifier the discovery
	// proposer will assign when emitting the lambda-cold-start-baseline
	// recommendation. The detection branch appends ".cold_start" to
	// this when consulting ExclusionStore so operators may exclude the
	// cold-start kind without excluding the serverless-tier kinds that
	// share the per-row recommendation root.
	RecommendationID string

	// Provider is "aws" in slice 1. Threaded onto the picker context so
	// the per-cloud Terraform shape is selected (slice 2 candidate).
	Provider string

	// Surface is "lambda" in slice 1. Slice 2 may add "cloudrun" /
	// "cloudfunc" / "azfunc" / "ocifunc" when GCP / Azure / OCI
	// cold-start lands.
	Surface string

	// ResourceTFName is the operator's best-effort Terraform resource
	// name. Empty when the IaC introspection couldn't classify; the
	// picker's snippet falls back to "<name>".
	ResourceTFName string

	// ResourceID is the canonical resource identifier (the Lambda
	// function ARN). Threaded into the reasoning text so the operator
	// sees which row Squadron flagged.
	ResourceID string

	// Region is the per-row region. Surfaced on the draft so the
	// downstream RecommendationDraft.Scope is well-formed.
	Region string
}

// ColdStartScope is the (connection, account/scope, region) tuple used
// to consult the exclusion store. Mirrors the SpanQualityScope shape so
// the wiring layer threads one scope tuple through all three detection
// branches (trace-emission / span-quality / cold-start) without
// re-projecting.
type ColdStartScope struct {
	ConnectionID string
	ScopeID      string
	Region       string
}

// ColdStartDetectionFinding is the slim projection of the chunk-2
// internal/discovery/aws.ColdStartDetectionResult the detection branch
// reads. Stated as a struct here (rather than importing the aws
// package) so this package stays disjoint from the cloud-specific
// scanner — the wiring layer projects ColdStartDetectionResult into
// this shape at the handler boundary.
//
// All three sub-rules from cold_start.go::ShouldFireRecommendation are
// pre-computed on the finding:
//
//  1. ExceedsThreshold — current is at least 1.5x baseline
//  2. ExceedsFloor — current is at least 500ms
//  3. BaselineTrustworthy — baseline sample count >= the chunk-2
//     ColdStartBaselineMinimumSamples constant
//
// The branch fires when all three are true; the wiring layer can
// reuse the chunk-2 ColdStartDetectionResult.ShouldFireRecommendation
// directly to set ShouldFire here, or compute it on the spot from the
// per-row stats it just queried.
type ColdStartDetectionFinding struct {
	// ShouldFire is the pre-computed canonical predicate. The branch
	// short-circuits on false to keep the detection logic mechanical.
	ShouldFire bool

	// CurrentP95Ms / BaselineP95Ms / Ratio are surfaced verbatim in
	// the reasoning template so the operator sees the exact numbers
	// without round-tripping through CloudWatch.
	CurrentP95Ms  float64
	BaselineP95Ms float64
	Ratio         float64

	// CurrentSampleCount / BaselineSampleCount are surfaced in the
	// reasoning so an operator who wants to double-check the
	// statistical posture can see the underlying volume. The chunk-2
	// detection enforces a minimum baseline sample count via
	// ShouldFireRecommendation; the values flow through here for
	// audit-trail completeness.
	CurrentSampleCount  int
	BaselineSampleCount int
}

// ColdStartRecommendationDraft is the per-row output of the cold-start
// detection branch. Mirrors SpanQualityRecommendationDraft —
// projected at the handler boundary into recommendations.Recommendation,
// kept separate here so the proposer package doesn't pull the
// recommendations package (circular dependency through services).
type ColdStartRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
	PickedPattern    iacpicker.PickedPattern
}

// ColdStartExclusionStore is the slice of ApplicationStore the cold-
// start detection branch consults. Same posture as
// SpanQualityExclusionStore + TraceEmissionExclusionStore — all three
// interfaces are satisfied by the same applicationstore.ApplicationStore
// so production wiring passes one store to all three branches.
type ColdStartExclusionStore interface {
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// ColdStartRecommendationKind is the single recommendation kind the
// cold-start detection branch emits in slice 1. Lifted into a named
// const so the per-row exclusion check + the prompt parity tests bind
// to the same identifier.
const ColdStartRecommendationKind = "lambda-cold-start-baseline"

// CheckLambdaColdStart is the §8 detection branch the discovery
// proposer flow runs ALONGSIDE the serverless tier kinds. For each
// Lambda inventory row whose chunk-2 cold-start finding fires (all
// three sub-rules hold), the branch emits a lambda-cold-start-baseline
// draft.
//
// Returns nil when:
//   - finding is nil OR ShouldFire is false (the chunk-2 detection
//     decided the regression isn't trustworthy / large enough),
//   - the row has been excluded by the operator (per-row or
//     kind-only marker),
//   - the surface isn't "lambda" (slice 1 AWS-only — GCP / Azure /
//     OCI cold-start ships in slice 2).
//
// Non-nil error means the exclusion store call failed; the caller
// (the wiring layer batch wrapper) accumulates per-row errors so one
// transient failure doesn't drop the whole pass.
func CheckLambdaColdStart(
	ctx context.Context,
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFinding,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
) (*ColdStartRecommendationDraft, error) {
	if finding == nil || !finding.ShouldFire {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != "lambda" {
		// Slice 1 cold-start covers AWS Lambda only. Surface other
		// than "lambda" is the slice 2 path; emit nothing here so the
		// chunk 3 detection stays surgical and slice 2 owns the
		// future GCP / Azure / OCI dispatch cleanly.
		return nil, nil
	}

	recID := row.RecommendationID + ".cold_start"
	if exclusions != nil && scope.ConnectionID != "" && scope.ScopeID != "" {
		excluded, err := exclusions.ListExcludedRecommendations(
			ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
		)
		if err != nil {
			return nil, fmt.Errorf("cold start: list excluded recommendations: %w", err)
		}
		for _, ex := range excluded {
			// Mirror trace-emission + span-quality posture: either the
			// row-level recommendation_id matches verbatim, OR the
			// kind-only marker (RecommendationID="" +
			// RecommendationKind set) covers the whole scope.
			if ex.RecommendationID != "" && ex.RecommendationID == recID {
				return nil, nil
			}
			if ex.RecommendationID == "" && ex.RecommendationKind == ColdStartRecommendationKind {
				return nil, nil
			}
		}
	}

	picked := iacpicker.PickColdStartProvisionedConcurrencyPattern(iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           "compute", // Lambda sits on the compute axis for picker dispatch.
		ResourceTFName: row.ResourceTFName,
	})
	return &ColdStartRecommendationDraft{
		Kind:             ColdStartRecommendationKind,
		RecommendationID: recID,
		Reasoning:        formatColdStartReasoning(row, finding, picked),
		Terraform:        picked.PrimaryTerraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
		PickedPattern:    picked,
	}, nil
}

// CheckLambdaColdStartBatch is the convenience wrapper that runs
// CheckLambdaColdStart over a slice of rows + per-row findings and
// accumulates the non-nil drafts. Errors are accumulated per row so a
// transient exclusion-store failure on one row doesn't drop the rest.
//
// findings[i] is paired with rows[i] by index; a nil finding entry is
// treated the same as "no cold-start observation for this row" (no
// draft). The caller (the wiring layer) is responsible for the row ->
// finding projection from the cold_start_observation latest-row query
// pair (24h + 168h windows).
func CheckLambdaColdStartBatch(
	ctx context.Context,
	rows []ColdStartInventoryRow,
	findings []*ColdStartDetectionFinding,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
) ([]ColdStartRecommendationDraft, []error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]ColdStartRecommendationDraft, 0, len(rows))
	var errs []error
	for i, row := range rows {
		var find *ColdStartDetectionFinding
		if i < len(findings) {
			find = findings[i]
		}
		draft, err := CheckLambdaColdStart(ctx, row, find, scope, exclusions)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if draft != nil {
			out = append(out, *draft)
		}
	}
	return out, errs
}

// formatColdStartReasoning composes the operator-facing reasoning for
// the lambda-cold-start-baseline kind. Pairs the §8 three-failure-mode
// framing with the per-row numbers (current p95 / baseline p95 / ratio
// + sample counts) so the operator can decline cleanly when their case
// is one of the alternatives (init-script regression / architecture
// change).
func formatColdStartReasoning(row ColdStartInventoryRow, finding *ColdStartDetectionFinding, picked iacpicker.PickedPattern) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This Lambda function's 24-hour P95 cold-start duration is %.0fms, %.2fx its 7-day baseline of %.0fms (current samples=%d, baseline samples=%d).\n\n",
		finding.CurrentP95Ms, finding.Ratio, finding.BaselineP95Ms,
		finding.CurrentSampleCount, finding.BaselineSampleCount,
	)
	fmt.Fprintf(&b,
		"Resource: %s (provider=%s, region=%s).\n\n",
		row.ResourceID, row.Provider, row.Region,
	)
	b.WriteString("Squadron flags this when the ratio exceeds 1.5x AND the absolute value exceeds 500ms. Three common causes — pick the one matching your deployment history:\n")
	b.WriteString("  1. Init script regression: a recent deployment added heavy imports / startup work. Compare deployment timeline to the regression onset.\n")
	b.WriteString("  2. Cold-start frequency increase: reduced invocation rate means more invocations hit the cold path. Consider provisioned concurrency for predictable traffic.\n")
	b.WriteString("  3. Architecture change: migration between architectures (x86_64 -> arm64) or runtime updates can shift cold-start behavior.\n\n")
	b.WriteString("This Terraform PR drafts a baseline provisioned concurrency configuration (floor=1, operator tunes). Decline if the cause is (1) or (3) and trace the regression in deployment history / architecture change intent. The verdict learning loop will record the decline.\n")
	if picked.Reasoning != "" {
		b.WriteString("\nPicker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------
// Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream
// 157) — per-cloud detection branches sibling to CheckLambdaColdStart.
// One helper per surface (cloudrun / cloudfunc / azfunc / ocifunc),
// each emitting a per-cloud cold-start-baseline draft when the
// underlying chunk-2/3 detection finding fires.
//
// Each per-cloud helper:
//   - short-circuits on nil finding OR !ShouldFire
//   - applies the per-row exclusion check (with the same ".cold_start"
//     suffix convention slice 1 introduced)
//   - calls the matching iacpicker emitter to get the Terraform +
//     reasoning hedge
//   - composes per-cloud reasoning text reflecting the per-surface
//     caveats (warm-path inclusion on GCP, IsAfterColdStart fallback
//     on Azure, function_duration approximation on OCI)
//
// See docs/proposals/cold-start-latency-slice2.md §3 + §8.
// ---------------------------------------------------------------------

// ColdStartRecommendationKindCloudRun is the recommendation kind the
// CheckCloudRunColdStart branch emits.
const ColdStartRecommendationKindCloudRun = "cloudrun-cold-start-baseline"

// ColdStartRecommendationKindCloudFunc is the recommendation kind the
// CheckCloudFunctionsColdStart branch emits.
const ColdStartRecommendationKindCloudFunc = "cloudfunc-cold-start-baseline"

// ColdStartRecommendationKindAzureFunc is the recommendation kind the
// CheckAzureFunctionsColdStart branch emits.
const ColdStartRecommendationKindAzureFunc = "azfunc-cold-start-baseline"

// ColdStartRecommendationKindOCIFunc is the recommendation kind the
// CheckOCIFunctionsColdStart branch emits.
const ColdStartRecommendationKindOCIFunc = "ocifunc-cold-start-baseline"

// ColdStartDetectionFindingPerCloud is the slim projection of the
// per-cloud chunk-1/2/3 ColdStartDetectionResult the slice 2 detection
// branches read. Stated as a struct here (rather than importing each
// cloud's scanner package) so this package stays disjoint from the
// cloud-specific scanners — the wiring layer projects each cloud's
// ColdStartDetectionResult into this shape at the handler boundary.
//
// All four sub-fields from the per-cloud ShouldFireRecommendation
// predicates are baked into ShouldFire on the wire so the branch just
// reads the flag. The other fields are surfaced verbatim in reasoning
// text.
type ColdStartDetectionFindingPerCloud struct {
	// ShouldFire is the pre-computed canonical predicate combining
	// ExceedsThreshold + ExceedsFloor + BaselineSampleCount >=
	// minimum, plus per-cloud gates (OCI Skipped).
	ShouldFire bool

	// CurrentP95Ms / BaselineP95Ms / Ratio are surfaced verbatim in
	// the reasoning template so the operator sees the exact numbers.
	CurrentP95Ms  float64
	BaselineP95Ms float64
	Ratio         float64

	// CurrentSampleCount / BaselineSampleCount surfaced for the
	// audit-trail completeness reasoning text.
	CurrentSampleCount  int
	BaselineSampleCount int

	// UsedFallback signals the Azure runtime didn't emit the
	// IsAfterColdStart dimension — Squadron fell back to an
	// unfiltered query and the reasoning text adds an informational
	// note explaining the operator's runtime version may need
	// upgrading for cold-start-isolated metrics. Only meaningful for
	// the Azure helper; the GCP / OCI helpers ignore this field.
	UsedFallback bool
}

// checkPerCloudColdStartExcluded consults the exclusion store for a
// row + scope and returns true when either the per-row marker or
// the kind-only marker covers the draft. Shared across the four per-
// cloud helpers so the exclusion semantics stay byte-identical.
func checkPerCloudColdStartExcluded(
	ctx context.Context,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
	kind, recID string,
) (bool, error) {
	if exclusions == nil || scope.ConnectionID == "" || scope.ScopeID == "" {
		return false, nil
	}
	excluded, err := exclusions.ListExcludedRecommendations(
		ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
	)
	if err != nil {
		return false, fmt.Errorf("cold start: list excluded recommendations: %w", err)
	}
	for _, ex := range excluded {
		if ex.RecommendationID != "" && ex.RecommendationID == recID {
			return true, nil
		}
		if ex.RecommendationID == "" && ex.RecommendationKind == kind {
			return true, nil
		}
	}
	return false, nil
}

// CheckCloudRunColdStart is the per-cloud detection branch for the
// cloudrun-cold-start-baseline kind. Sibling of CheckLambdaColdStart.
// Returns nil when:
//   - finding is nil OR ShouldFire is false,
//   - the row has been excluded by the operator,
//   - the surface isn't "cloudrun".
func CheckCloudRunColdStart(
	ctx context.Context,
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
) (*ColdStartRecommendationDraft, error) {
	if finding == nil || !finding.ShouldFire {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != "cloudrun" {
		return nil, nil
	}
	recID := row.RecommendationID + ".cold_start"
	excluded, err := checkPerCloudColdStartExcluded(ctx, scope, exclusions, ColdStartRecommendationKindCloudRun, recID)
	if err != nil {
		return nil, err
	}
	if excluded {
		return nil, nil
	}
	picked := iacpicker.PickCloudRunColdStartPattern(iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           "serverless",
		ResourceTFName: row.ResourceTFName,
	})
	return &ColdStartRecommendationDraft{
		Kind:             ColdStartRecommendationKindCloudRun,
		RecommendationID: recID,
		Reasoning:        formatCloudRunColdStartReasoning(row, finding, picked),
		Terraform:        picked.PrimaryTerraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
		PickedPattern:    picked,
	}, nil
}

// CheckCloudFunctionsColdStart is the per-cloud detection branch for
// the cloudfunc-cold-start-baseline kind.
func CheckCloudFunctionsColdStart(
	ctx context.Context,
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
) (*ColdStartRecommendationDraft, error) {
	if finding == nil || !finding.ShouldFire {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != "cloudfunc" {
		return nil, nil
	}
	recID := row.RecommendationID + ".cold_start"
	excluded, err := checkPerCloudColdStartExcluded(ctx, scope, exclusions, ColdStartRecommendationKindCloudFunc, recID)
	if err != nil {
		return nil, err
	}
	if excluded {
		return nil, nil
	}
	picked := iacpicker.PickCloudFunctionsColdStartPattern(iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           "serverless",
		ResourceTFName: row.ResourceTFName,
	})
	return &ColdStartRecommendationDraft{
		Kind:             ColdStartRecommendationKindCloudFunc,
		RecommendationID: recID,
		Reasoning:        formatCloudFunctionsColdStartReasoning(row, finding, picked),
		Terraform:        picked.PrimaryTerraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
		PickedPattern:    picked,
	}, nil
}

// CheckAzureFunctionsColdStart is the per-cloud detection branch for
// the azfunc-cold-start-baseline kind. When finding.UsedFallback is
// true, the reasoning text adds an informational note explaining
// Squadron fell back to an unfiltered query because the Function
// App's runtime didn't emit the IsAfterColdStart dimension.
func CheckAzureFunctionsColdStart(
	ctx context.Context,
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
) (*ColdStartRecommendationDraft, error) {
	if finding == nil || !finding.ShouldFire {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != "azfunc" {
		return nil, nil
	}
	recID := row.RecommendationID + ".cold_start"
	excluded, err := checkPerCloudColdStartExcluded(ctx, scope, exclusions, ColdStartRecommendationKindAzureFunc, recID)
	if err != nil {
		return nil, err
	}
	if excluded {
		return nil, nil
	}
	picked := iacpicker.PickAzureFunctionsColdStartPattern(iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           "serverless",
		ResourceTFName: row.ResourceTFName,
	})
	return &ColdStartRecommendationDraft{
		Kind:             ColdStartRecommendationKindAzureFunc,
		RecommendationID: recID,
		Reasoning:        formatAzureFunctionsColdStartReasoning(row, finding, picked),
		Terraform:        picked.PrimaryTerraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
		PickedPattern:    picked,
	}, nil
}

// CheckOCIFunctionsColdStart is the per-cloud detection branch for
// the ocifunc-cold-start-baseline kind.
func CheckOCIFunctionsColdStart(
	ctx context.Context,
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	scope ColdStartScope,
	exclusions ColdStartExclusionStore,
) (*ColdStartRecommendationDraft, error) {
	if finding == nil || !finding.ShouldFire {
		return nil, nil
	}
	if row.Surface != "" && row.Surface != "ocifunc" {
		return nil, nil
	}
	recID := row.RecommendationID + ".cold_start"
	excluded, err := checkPerCloudColdStartExcluded(ctx, scope, exclusions, ColdStartRecommendationKindOCIFunc, recID)
	if err != nil {
		return nil, err
	}
	if excluded {
		return nil, nil
	}
	picked := iacpicker.PickOCIFunctionsColdStartPattern(iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           "serverless",
		ResourceTFName: row.ResourceTFName,
	})
	return &ColdStartRecommendationDraft{
		Kind:             ColdStartRecommendationKindOCIFunc,
		RecommendationID: recID,
		Reasoning:        formatOCIFunctionsColdStartReasoning(row, finding, picked),
		Terraform:        picked.PrimaryTerraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
		PickedPattern:    picked,
	}, nil
}

// formatCloudRunColdStartReasoning composes the operator-facing
// reasoning for the cloudrun-cold-start-baseline kind. Mirrors the
// lambda 3-failure-mode framing and adds the GCP warm-path inclusion
// caveat from §3.1.
func formatCloudRunColdStartReasoning(
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	picked iacpicker.PickedPattern,
) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This Cloud Run service's 24-hour P95 request latency is %.0fms, %.2fx its 7-day baseline of %.0fms (current samples=%d, baseline samples=%d).\n\n",
		finding.CurrentP95Ms, finding.Ratio, finding.BaselineP95Ms,
		finding.CurrentSampleCount, finding.BaselineSampleCount,
	)
	fmt.Fprintf(&b, "Resource: %s (provider=%s, region=%s).\n\n", row.ResourceID, row.Provider, row.Region)
	b.WriteString("Squadron flags this when the ratio exceeds 1.5x AND the absolute value exceeds 500ms. CAVEAT: Cloud Run's request_latencies metric includes warm-path invocations, so a permanently-warm service (min-instances already set, regular traffic) may show false positives. Three common causes to evaluate:\n")
	b.WriteString("  1. Init script regression: a recent deployment added heavy imports / startup work. Compare deployment timeline to the regression onset.\n")
	b.WriteString("  2. Cold-start frequency increase: reduced invocation rate means more requests hit the cold path. Consider min-instances for predictable traffic.\n")
	b.WriteString("  3. Architecture change: container image / runtime updates can shift cold-start behavior.\n\n")
	b.WriteString("This Terraform PR drafts a baseline autoscaling.knative.dev/minScale annotation (floor=1, operator tunes). Decline if the cause is (1) or (3) and trace the regression in deployment history. The verdict learning loop will record the decline.\n")
	if picked.Reasoning != "" {
		b.WriteString("\nPicker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	return b.String()
}

// formatCloudFunctionsColdStartReasoning composes the reasoning for
// the cloudfunc-cold-start-baseline kind. Same caveat as Cloud Run
// (warm-path inclusion).
func formatCloudFunctionsColdStartReasoning(
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	picked iacpicker.PickedPattern,
) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This Cloud Function's 24-hour P95 execution time is %.0fms, %.2fx its 7-day baseline of %.0fms (current samples=%d, baseline samples=%d).\n\n",
		finding.CurrentP95Ms, finding.Ratio, finding.BaselineP95Ms,
		finding.CurrentSampleCount, finding.BaselineSampleCount,
	)
	fmt.Fprintf(&b, "Resource: %s (provider=%s, region=%s).\n\n", row.ResourceID, row.Provider, row.Region)
	b.WriteString("Squadron flags this when the ratio exceeds 1.5x AND the absolute value exceeds 500ms. CAVEAT: Cloud Functions' execution_times metric includes warm invocations, so functions on Gen 2 with min instances already set may show false positives. Three common causes to evaluate:\n")
	b.WriteString("  1. Init script regression: a recent deployment added heavy imports / startup work.\n")
	b.WriteString("  2. Cold-start frequency increase: reduced invocation rate means more invocations hit the cold path. Consider min_instance_count for predictable traffic.\n")
	b.WriteString("  3. Architecture change: runtime updates or memory-tier changes can shift cold-start behavior.\n\n")
	b.WriteString("This Terraform PR drafts a baseline min_instance_count = 1 on the Gen 2 service_config (operator tunes). Decline if the cause is (1) or (3) and trace the regression in deployment history.\n")
	if picked.Reasoning != "" {
		b.WriteString("\nPicker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	return b.String()
}

// formatAzureFunctionsColdStartReasoning composes the reasoning for
// the azfunc-cold-start-baseline kind. When finding.UsedFallback is
// true, the reasoning text adds an informational note explaining
// Squadron fell back to an unfiltered query because the Function
// App's runtime didn't emit the IsAfterColdStart dimension — the
// P95 reported is across ALL invocations (cold + warm) rather than
// cold-start-isolated. The regression signal is still actionable
// (the overall execution duration spiked) but the operator should
// know the metric isn't cold-start-isolated for older runtimes.
func formatAzureFunctionsColdStartReasoning(
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	picked iacpicker.PickedPattern,
) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This Azure Function App's 24-hour P95 execution duration is %.0fms, %.2fx its 7-day baseline of %.0fms (current samples=%d, baseline samples=%d).\n\n",
		finding.CurrentP95Ms, finding.Ratio, finding.BaselineP95Ms,
		finding.CurrentSampleCount, finding.BaselineSampleCount,
	)
	fmt.Fprintf(&b, "Resource: %s (provider=%s, region=%s).\n\n", row.ResourceID, row.Provider, row.Region)
	if finding.UsedFallback {
		b.WriteString("INFORMATIONAL NOTE: This Function App's runtime version did NOT emit the IsAfterColdStart dimension on FunctionExecutionDuration — the IsAfterColdStart dimension was introduced in 2023+ runtime versions. Squadron fell back to an unfiltered P95 query, so the value above is across ALL invocations (cold + warm), not cold-start-isolated. The regression signal is still actionable (overall execution duration spiked), but consider upgrading the Function App's runtime to get cold-start-isolated metrics in future scans.\n\n")
	}
	b.WriteString("Squadron flags this when the ratio exceeds 1.5x AND the absolute value exceeds 500ms. Three common causes to evaluate:\n")
	b.WriteString("  1. Init script regression: a recent deployment added heavy imports / startup work.\n")
	b.WriteString("  2. Cold-start frequency increase: Consumption-plan apps scale to zero between invocations; Premium Plan eliminates this.\n")
	b.WriteString("  3. Architecture change: runtime updates can shift cold-start behavior.\n\n")
	b.WriteString("This Terraform PR drafts two paths: Premium Plan migration (EP1+) eliminates cold-start by maintaining warm instances; the lighter-weight alternative disables placeholder mode (WEBSITE_USE_PLACEHOLDER = \"0\"). Pick based on cost tolerance. Decline if the cause is (1) or (3).\n")
	if picked.Reasoning != "" {
		b.WriteString("\nPicker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	return b.String()
}

// formatOCIFunctionsColdStartReasoning composes the reasoning for
// the ocifunc-cold-start-baseline kind. Notes the function_duration
// approximation honestly (it's not cold-start-isolated; the
// cold_start_count > 0 gate ensures the window genuinely had cold
// starts, but the P95 is across all those invocations).
func formatOCIFunctionsColdStartReasoning(
	row ColdStartInventoryRow,
	finding *ColdStartDetectionFindingPerCloud,
	picked iacpicker.PickedPattern,
) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"This OCI Function's 24-hour P95 FunctionExecutionDuration is %.0fms, %.2fx its 7-day baseline of %.0fms (current samples=%d, baseline samples=%d).\n\n",
		finding.CurrentP95Ms, finding.Ratio, finding.BaselineP95Ms,
		finding.CurrentSampleCount, finding.BaselineSampleCount,
	)
	fmt.Fprintf(&b, "Resource: %s (provider=%s, region=%s).\n\n", row.ResourceID, row.Provider, row.Region)
	b.WriteString("Squadron flags this when the current P95 exceeds 1.5x the baseline AND is above 500ms. CAVEAT: OCI's oci_faas namespace has no isolated cold-start latency metric (and no cold-start counter), so this is a FunctionExecutionDuration P95-regression heuristic — the P95 is across ALL invocations in the window, not cold-start-isolated. A spike may be a cold start OR a slow downstream dependency. Three common causes to evaluate:\n")
	b.WriteString("  1. Init script regression: a recent deployment added heavy imports / startup work.\n")
	b.WriteString("  2. Cold-start frequency increase: reduced invocation rate or scale-to-zero events.\n")
	b.WriteString("  3. Architecture change: runtime / memory updates can shift cold-start behavior.\n\n")
	b.WriteString("This Terraform PR drafts a WARMUP_DELAY config tuning adjustment. OCI Functions provisioned_concurrent_executions is currently in preview; when it exits preview the recommendation will shift to that. Decline if the cause is (1) or (3).\n")
	if picked.Reasoning != "" {
		b.WriteString("\nPicker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	return b.String()
}
