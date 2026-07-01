// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/iac"
	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/recommendations"
)

// Regression-recommendation wiring (detection → proposal). The cold-start +
// error-rate REGRESSION detectors persist observations and annotate the
// serverless inventory rows (ColdStartExceedsThreshold etc.), but the
// deterministic recommendation builders in internal/proposer
// (CheckLambdaColdStart + per-cloud / error-rate siblings) had no production
// caller — the detection dead-ended as a UI cell instead of becoming an
// actionable Terraform PR. This file closes that loop for the discovery
// recommendations flow.
//
// The core logic is cloud-agnostic: it iterates a slice of
// scanner.ServerlessInstanceSnapshot, gates on the detection annotations, and
// dispatches to the right per-surface builder. AWS feeds it by projecting its
// snake_case wire rows into snapshots; GCP/Azure/OCI feed their snapshots
// directly (their scan responses marshal the snapshot type verbatim). The recs
// only appear when a prior scan produced the detection annotations (commercial
// add-on enabled for AWS Lambda / Azure Functions; OSS-native for GCP Cloud
// Run / Cloud Functions + OCI Functions) — naturally gated by data
// availability, no extra flag.

// regressionCurrentWindowHours / regressionBaselineWindowHours are the
// observation windows both regression detectors (cold-start + error-rate)
// persist under; the recs path reads them back to reconstruct the detection
// result / enrich the reasoning text.
const (
	regressionCurrentWindowHours  = 24
	regressionBaselineWindowHours = 168
)

// errorRateBuilder is the shared shape of the per-surface error-rate proposer
// builders (all five take the same ErrorRateDetectionResult), so error-rate can
// dispatch by surface uniformly. Cold-start can't: AWS Lambda takes a
// ColdStartDetectionFinding while the other four take the richer
// ColdStartDetectionFindingPerCloud, so cold-start dispatch is a switch
// (dispatchColdStartBuilder) that builds the right finding type per surface.
type errorRateBuilder func(context.Context, proposer.ErrorRateInventoryRow, *proposer.ErrorRateDetectionResult, proposer.ErrorRateScope, proposer.ErrorRateExclusionStore) (*proposer.ErrorRateRecommendationDraft, error)

// dispatchColdStartBuilder constructs the surface-appropriate detection finding
// (Lambda's ColdStartDetectionFinding vs the per-cloud variant) from the shared
// metric values and calls the matching builder. An unknown surface returns
// (nil, nil) so the row is skipped — a new surface can't silently emit a
// mis-shaped recommendation.
func dispatchColdStartBuilder(
	ctx context.Context, surface string, row proposer.ColdStartInventoryRow,
	currentP95, baselineP95, ratio float64, currentSamples, baselineSamples int,
	scope proposer.ColdStartScope, exclusions proposer.ColdStartExclusionStore,
) (*proposer.ColdStartRecommendationDraft, error) {
	if surface == "lambda" {
		f := proposer.ColdStartDetectionFinding{
			ShouldFire: true, CurrentP95Ms: currentP95, BaselineP95Ms: baselineP95,
			Ratio: ratio, CurrentSampleCount: currentSamples, BaselineSampleCount: baselineSamples,
		}
		return proposer.CheckLambdaColdStart(ctx, row, &f, scope, exclusions)
	}
	f := proposer.ColdStartDetectionFindingPerCloud{
		ShouldFire: true, CurrentP95Ms: currentP95, BaselineP95Ms: baselineP95,
		Ratio: ratio, CurrentSampleCount: currentSamples, BaselineSampleCount: baselineSamples,
	}
	switch surface {
	case "cloudrun":
		return proposer.CheckCloudRunColdStart(ctx, row, &f, scope, exclusions)
	case "cloudfunc":
		return proposer.CheckCloudFunctionsColdStart(ctx, row, &f, scope, exclusions)
	case "azfunc":
		return proposer.CheckAzureFunctionsColdStart(ctx, row, &f, scope, exclusions)
	case "ocifunc":
		return proposer.CheckOCIFunctionsColdStart(ctx, row, &f, scope, exclusions)
	default:
		return nil, nil
	}
}

// errorRateBuilderForSurface maps a serverless surface to its error-rate
// builder (all five share the span-quality-error-rate-spike kind).
func errorRateBuilderForSurface(surface string) errorRateBuilder {
	switch surface {
	case "lambda":
		return proposer.CheckLambdaErrorRate
	case "cloudrun":
		return proposer.CheckCloudRunErrorRate
	case "cloudfunc":
		return proposer.CheckCloudFunctionsErrorRate
	case "azfunc":
		return proposer.CheckAzureFunctionsErrorRate
	case "ocifunc":
		return proposer.CheckOCIFunctionsErrorRate
	default:
		return nil
	}
}

// appendColdStartRegressionRecs appends one cold-start regression
// recommendation per serverless row whose canonical exceeds-threshold flag is
// set. Best-effort + additive: a nil store, missing observations, an unknown
// surface, or a builder error never blocks the LLM recs already in `recs`.
// Exclusions are honored inside the builder.
func appendColdStartRegressionRecs(
	ctx context.Context,
	recs *[]recommendations.Recommendation,
	serverless []scanner.ServerlessInstanceSnapshot,
	coldStartStore ColdStartObservationReader,
	exclusions proposer.ColdStartExclusionStore,
	scope proposer.ColdStartScope,
	scanID string,
	now time.Time,
	logger *zap.Logger,
) {
	for _, sv := range serverless {
		if sv.ResourceARN == "" {
			continue
		}
		// Gate on the canonical operator-facing predicate already computed by
		// the cold-start annotation pass — zero threshold drift.
		if sv.ColdStartExceedsThreshold == nil || !*sv.ColdStartExceedsThreshold {
			continue
		}
		var currentP95, baselineP95, ratio float64
		var currentSamples, baselineSamples int
		if sv.ColdStartP95Ms != nil {
			currentP95 = *sv.ColdStartP95Ms
		}
		// Enrich baseline + ratio + sample counts from the observation store for
		// the reasoning text. Optional — absence just yields a terser reasoning
		// string, never a dropped recommendation.
		if coldStartStore != nil {
			if base, ok, err := coldStartStore.LatestColdStartObservation(ctx, sv.ResourceARN, regressionBaselineWindowHours); err == nil && ok {
				baselineP95 = base.P95Ms
				baselineSamples = base.SampleCount
				if base.P95Ms > 0 {
					ratio = currentP95 / base.P95Ms
				}
			}
			if cur, ok, err := coldStartStore.LatestColdStartObservation(ctx, sv.ResourceARN, regressionCurrentWindowHours); err == nil && ok {
				currentSamples = cur.SampleCount
			}
		}
		row := proposer.ColdStartInventoryRow{
			RecommendationID: sv.ResourceARN, // stable across scans → exclusions persist
			Provider:         sv.Provider,
			Surface:          sv.Surface,
			ResourceTFName:   sv.ResourceName,
			ResourceID:       sv.ResourceARN,
			Region:           sv.Region,
		}
		draft, err := dispatchColdStartBuilder(ctx, sv.Surface, row,
			currentP95, baselineP95, ratio, currentSamples, baselineSamples, scope, exclusions)
		if err != nil {
			if logger != nil {
				logger.Warn("cold-start regression rec build error", zap.Error(err), zap.String("surface", sv.Surface))
			}
			continue
		}
		if draft == nil {
			continue // gate not met or excluded.
		}
		*recs = append(*recs, coldStartDraftToRecommendation(*draft, scanID, now))
	}
}

// appendErrorRateRegressionRecs appends one error-rate-spike recommendation per
// serverless row whose persisted error-rate observations clear the canonical
// gates (rate ratio > 2.0x AND >= 1000 invocations AND >= 50 errors in the 24h
// window). The detection result is reconstructed from the error_rate_observation
// store (24h current + 168h baseline) and re-gated via the shared
// proposer.FinalizeErrorRateGates so the thresholds never drift from live
// detection. A snapshot whose error-rate flag is explicitly false is skipped
// before the store lookup (an optimization; AWS rows carry no such flag and
// fall through to the store + the builder's own gate). Best-effort + additive;
// exclusions honored inside the builder.
func appendErrorRateRegressionRecs(
	ctx context.Context,
	recs *[]recommendations.Recommendation,
	serverless []scanner.ServerlessInstanceSnapshot,
	errorRateStore ErrorRateObservationStore,
	exclusions proposer.ErrorRateExclusionStore,
	scope proposer.ErrorRateScope,
	scanID string,
	now time.Time,
	logger *zap.Logger,
) {
	if errorRateStore == nil {
		return // read store not wired → nothing to reconstruct from.
	}
	for _, sv := range serverless {
		if sv.ResourceARN == "" {
			continue
		}
		// Skip rows the annotation pass already cleared (flag present + false).
		// Rows with a nil flag (e.g. AWS, which doesn't carry it on the wire)
		// fall through; the builder's ShouldFireRecommendation is the real gate.
		if sv.ErrorRateExceedsThreshold != nil && !*sv.ErrorRateExceedsThreshold {
			continue
		}
		build := errorRateBuilderForSurface(sv.Surface)
		if build == nil {
			continue
		}
		cur, okCur, errCur := errorRateStore.LatestErrorRateObservation(ctx, sv.ResourceARN, regressionCurrentWindowHours)
		base, okBase, errBase := errorRateStore.LatestErrorRateObservation(ctx, sv.ResourceARN, regressionBaselineWindowHours)
		if errCur != nil || errBase != nil || !okCur || !okBase {
			continue
		}
		result := proposer.ErrorRateDetectionResult{
			ResourceARN:             sv.ResourceARN,
			Surface:                 sv.Surface,
			CurrentInvocationCount:  uint64(cur.InvocationCount),
			CurrentErrorCount:       uint64(cur.ErrorCount),
			BaselineInvocationCount: uint64(base.InvocationCount),
			BaselineErrorCount:      uint64(base.ErrorCount),
		}
		proposer.FinalizeErrorRateGates(&result) // derives rates + the three gate booleans
		row := proposer.ErrorRateInventoryRow{
			RecommendationID: sv.ResourceARN, // stable across scans → exclusions persist
			Provider:         sv.Provider,
			Surface:          sv.Surface,
			ResourceTFName:   sv.ResourceName,
			ResourceID:       sv.ResourceARN,
			Region:           sv.Region,
		}
		draft, err := build(ctx, row, &result, scope, exclusions)
		if err != nil {
			if logger != nil {
				logger.Warn("error-rate regression rec build error", zap.Error(err), zap.String("surface", sv.Surface))
			}
			continue
		}
		if draft == nil {
			continue // gates not met or excluded.
		}
		*recs = append(*recs, errorRateDraftToRecommendation(*draft, scanID, now))
	}
}

// samplingBuilder is the shared shape of the per-surface sampling-rate proposer
// builders (all five take the same SamplingRateDetectionResult + row + scope +
// exclusions), so sampling can dispatch by surface uniformly — like error-rate,
// and unlike cold-start (which needs per-surface finding types).
type samplingBuilder func(context.Context, proposer.SamplingRateInventoryRow, *proposer.SamplingRateDetectionResult, proposer.SamplingRateScope, proposer.SamplingRateExclusionStore) (*proposer.SamplingRateRecommendationDraft, error)

// samplingBuilderForSurface maps a serverless surface to its sampling-rate
// builder (all five share the span-quality-sampling-too-aggressive kind).
func samplingBuilderForSurface(surface string) samplingBuilder {
	switch surface {
	case "lambda":
		return proposer.CheckLambdaSamplingRate
	case "cloudrun":
		return proposer.CheckCloudRunSamplingRate
	case "cloudfunc":
		return proposer.CheckCloudFunctionsSamplingRate
	case "azfunc":
		return proposer.CheckAzureFunctionsSamplingRate
	case "ocifunc":
		return proposer.CheckOCIFunctionsSamplingRate
	default:
		return nil
	}
}

// appendSamplingRegressionRecs appends one sampling-too-aggressive
// recommendation per serverless row whose most-recent live sampling result
// (below the 5% floor AND >= 1000 invocations) is held in the sampling cache.
// Unlike cold-start / error-rate — which reconstruct a detection result from a
// time-series sqlite observation store — sampling is a LIVE join recomputed
// every scan (cloud invocation count ⋈ traceindex span count), so the backing
// is the in-memory SamplingObservationCache the scan-response annotation pass
// filled (samplingReader, a SamplingDetector). A nil reader, a cache miss
// (zero-value result → ShouldFire false), an unknown surface, or a builder
// error never blocks the recs already in `recs`. Exclusions honored inside the
// builder. A row whose SamplingExceedsFloor flag is explicitly false is skipped
// before the lookup; a nil flag falls through (the cache + builder gate).
func appendSamplingRegressionRecs(
	ctx context.Context,
	recs *[]recommendations.Recommendation,
	serverless []scanner.ServerlessInstanceSnapshot,
	samplingReader SamplingDetector,
	exclusions proposer.SamplingRateExclusionStore,
	scope proposer.SamplingRateScope,
	scanID string,
	now time.Time,
	logger *zap.Logger,
) {
	if samplingReader == nil {
		return // no live-sampling cache wired → nothing to reconstruct from.
	}
	for _, sv := range serverless {
		if sv.ResourceARN == "" {
			continue
		}
		// Skip rows the annotation pass already cleared (flag present + false).
		// A nil flag falls through — the cache lookup + builder gate decide.
		if sv.SamplingExceedsFloor != nil && !*sv.SamplingExceedsFloor {
			continue
		}
		build := samplingBuilderForSurface(sv.Surface)
		if build == nil {
			continue
		}
		// The traceindex join key for serverless is the resource ARN (same as
		// the annotation pass + the per-resource endpoint). The cache resolves
		// purely by ARN; surface + key are informational.
		result, err := samplingReader.DetectSampling(ctx, sv.ResourceARN, sv.Surface, sv.ResourceARN)
		if err != nil {
			if logger != nil {
				logger.Warn("sampling regression rec lookup error", zap.Error(err), zap.String("surface", sv.Surface))
			}
			continue
		}
		row := proposer.SamplingRateInventoryRow{
			RecommendationID: sv.ResourceARN, // stable across scans → exclusions persist
			Provider:         sv.Provider,
			Surface:          sv.Surface,
			ResourceTFName:   sv.ResourceName,
			ResourceID:       sv.ResourceARN,
			Region:           sv.Region,
		}
		draft, err := build(ctx, row, &result, scope, exclusions)
		if err != nil {
			if logger != nil {
				logger.Warn("sampling regression rec build error", zap.Error(err), zap.String("surface", sv.Surface))
			}
			continue
		}
		if draft == nil {
			continue // gate not met or excluded.
		}
		*recs = append(*recs, samplingDraftToRecommendation(*draft, scanID, now))
	}
}

// appendRegressionRecs runs all three regression-rec passes (cold-start +
// error-rate + sampling) over a serverless snapshot slice for one connection
// scope. The per-cloud recs handlers call this directly with their connection
// identity, so they need no proposer import. exclusions is a
// DiscoveryExclusionStore (the app store) which structurally satisfies all
// three proposer exclusion interfaces. samplingReader is the per-server
// SamplingObservationCache (nil-safe: a nil reader just skips the sampling
// pass).
func appendRegressionRecs(
	ctx context.Context,
	recs *[]recommendations.Recommendation,
	serverless []scanner.ServerlessInstanceSnapshot,
	coldStartStore ColdStartObservationReader,
	errorRateStore ErrorRateObservationStore,
	samplingReader SamplingDetector,
	exclusions DiscoveryExclusionStore,
	connectionID, scopeID, region, scanID string,
	now time.Time,
	logger *zap.Logger,
) {
	appendColdStartRegressionRecs(ctx, recs, serverless, coldStartStore, exclusions,
		proposer.ColdStartScope{ConnectionID: connectionID, ScopeID: scopeID, Region: region},
		scanID, now, logger)
	appendErrorRateRegressionRecs(ctx, recs, serverless, errorRateStore, exclusions,
		proposer.ErrorRateScope{ConnectionID: connectionID, ScopeID: scopeID, Region: region},
		scanID, now, logger)
	appendSamplingRegressionRecs(ctx, recs, serverless, samplingReader, exclusions,
		proposer.SamplingRateScope{ConnectionID: connectionID, ScopeID: scopeID, Region: region},
		scanID, now, logger)
}

// --- AWS adapters: project the snake_case wire rows into snapshots and
// delegate to the cloud-agnostic helpers above. ----------------------------

// awsServerlessRowsToSnapshots projects the AWS wire rows into the shared
// snapshot type the regression-recs helpers consume. Only the fields the
// helpers read are carried (the error-rate axis is absent on the AWS wire row,
// so its ErrorRateExceedsThreshold stays nil → the store + builder gate it).
func awsServerlessRowsToSnapshots(rows []awsServerlessRow) []scanner.ServerlessInstanceSnapshot {
	out := make([]scanner.ServerlessInstanceSnapshot, 0, len(rows))
	for _, sv := range rows {
		out = append(out, scanner.ServerlessInstanceSnapshot{
			Provider:                  sv.Provider,
			Surface:                   sv.Surface,
			Region:                    sv.Region,
			ResourceName:              sv.ResourceName,
			ResourceARN:               sv.ResourceARN,
			ColdStartP95Ms:            sv.ColdStartP95Ms,
			ColdStartExceedsThreshold: sv.ColdStartExceedsThreshold,
			// SamplingExceedsFloor rides the AWS wire row (the annotation
			// pass stamps it); carry it so the sampling regression pre-filter
			// can gate on it. Cold-start/error-rate axes stay as-is.
			SamplingExceedsFloor: sv.SamplingExceedsFloor,
		})
	}
	return out
}

func (h *DiscoveryHandlers) appendAWSColdStartRegressionRecs(
	ctx context.Context, recs *[]recommendations.Recommendation, scan awsScanResponse, now time.Time,
) {
	scope := proposer.ColdStartScope{
		ConnectionID: scan.AccountID, // AWS: connectionID == accountID
		ScopeID:      scan.AccountID,
		Region:       firstRegion(scan.Regions),
	}
	appendColdStartRegressionRecs(ctx, recs, awsServerlessRowsToSnapshots(scan.Serverless),
		h.coldStartStore, h.exclusionStore, scope, scan.ScanID, now, h.logger)
}

func (h *DiscoveryHandlers) appendAWSErrorRateRegressionRecs(
	ctx context.Context, recs *[]recommendations.Recommendation, scan awsScanResponse, now time.Time,
) {
	scope := proposer.ErrorRateScope{
		ConnectionID: scan.AccountID,
		ScopeID:      scan.AccountID,
		Region:       firstRegion(scan.Regions),
	}
	appendErrorRateRegressionRecs(ctx, recs, awsServerlessRowsToSnapshots(scan.Serverless),
		h.errorRateStore, h.exclusionStore, scope, scan.ScanID, now, h.logger)
}

func (h *DiscoveryHandlers) appendAWSSamplingRegressionRecs(
	ctx context.Context, recs *[]recommendations.Recommendation, scan awsScanResponse, now time.Time,
) {
	scope := proposer.SamplingRateScope{
		ConnectionID: scan.AccountID,
		ScopeID:      scan.AccountID,
		Region:       firstRegion(scan.Regions),
	}
	appendSamplingRegressionRecs(ctx, recs, awsServerlessRowsToSnapshots(scan.Serverless),
		h.samplingSink, h.exclusionStore, scope, scan.ScanID, now, h.logger)
}

// --- draft → wire envelope conversions (shared across clouds) --------------

// coldStartDraftToRecommendation maps a proposer.ColdStartRecommendationDraft
// onto the wire recommendation envelope, mirroring buildDiscoveryRecommendations
// so the regression rec renders + opens a PR exactly like an LLM-proposed step.
func coldStartDraftToRecommendation(
	d proposer.ColdStartRecommendationDraft, scanID string, now time.Time,
) recommendations.Recommendation {
	rec := recommendations.Recommendation{
		ID:          d.RecommendationID,
		Category:    recommendations.CategoryEmptySignal,
		Severity:    recommendations.SeverityWarn,
		Title:       "Cold-start latency regression",
		Detail:      d.Reasoning,
		GeneratedAt: now,
		Source: &recommendations.RecommendationSource{
			Kind:  recommendations.SourceDiscoveryScan,
			RefID: scanID,
		},
		IaC: &recommendations.IaCSnippet{
			Format: recommendations.IaCTerraform,
			Source: d.Terraform,
		},
		ResourceKind:      d.Kind, // e.g. "lambda-cold-start-baseline" / "cloudrun-cold-start-baseline"
		AffectedResources: []string{d.ResourceID},
	}
	if rec.ResourceKind != "" {
		rec.Disposition = iac.DispositionFor(rec.ResourceKind)
	}
	return rec
}

// errorRateDraftToRecommendation maps a proposer.ErrorRateRecommendationDraft
// onto the wire recommendation envelope (mirrors coldStartDraftToRecommendation).
func errorRateDraftToRecommendation(
	d proposer.ErrorRateRecommendationDraft, scanID string, now time.Time,
) recommendations.Recommendation {
	rec := recommendations.Recommendation{
		ID:          d.RecommendationID,
		Category:    recommendations.CategoryEmptySignal,
		Severity:    recommendations.SeverityWarn,
		Title:       "Elevated error rate",
		Detail:      d.Reasoning,
		GeneratedAt: now,
		Source: &recommendations.RecommendationSource{
			Kind:  recommendations.SourceDiscoveryScan,
			RefID: scanID,
		},
		IaC: &recommendations.IaCSnippet{
			Format: recommendations.IaCTerraform,
			Source: d.Terraform,
		},
		ResourceKind:      d.Kind, // "span-quality-error-rate-spike"
		AffectedResources: []string{d.ResourceID},
	}
	if rec.ResourceKind != "" {
		rec.Disposition = iac.DispositionFor(rec.ResourceKind)
	}
	return rec
}

// samplingDraftToRecommendation maps a proposer.SamplingRateRecommendationDraft
// onto the wire recommendation envelope (mirrors errorRateDraftToRecommendation).
func samplingDraftToRecommendation(
	d proposer.SamplingRateRecommendationDraft, scanID string, now time.Time,
) recommendations.Recommendation {
	rec := recommendations.Recommendation{
		ID:          d.RecommendationID,
		Category:    recommendations.CategoryEmptySignal,
		Severity:    recommendations.SeverityWarn,
		Title:       "Sampling too aggressive",
		Detail:      d.Reasoning,
		GeneratedAt: now,
		Source: &recommendations.RecommendationSource{
			Kind:  recommendations.SourceDiscoveryScan,
			RefID: scanID,
		},
		IaC: &recommendations.IaCSnippet{
			Format: recommendations.IaCTerraform,
			Source: d.Terraform,
		},
		ResourceKind:      d.Kind, // "span-quality-sampling-too-aggressive"
		AffectedResources: []string{d.ResourceID},
	}
	if rec.ResourceKind != "" {
		rec.Disposition = iac.DispositionFor(rec.ResourceKind)
	}
	return rec
}
