// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/proposer"
)

// inventory_sampling.go — Sampling rate analysis slice 1 chunk 2
// (v0.89.123, #763 Stream 161). Sibling of inventory_cold_start.go:
// per-provider scan handlers call AnnotateServerlessWithSampling
// AFTER the scanner produces a Result and AFTER the trace-emission
// LastSeenAt + cold-start annotations, BEFORE marshalScanResult
// serializes the response. The helper:
//   - Returns immediately when the supplied SamplingAnnotator or
//     SamplingKeyResolver is nil (sampling detection disabled in
//     this deployment, or the per-cloud MetricQuerier substrate
//     isn't wired).
//   - Iterates the supplied serverless snapshots, skipping any
//     with surface not in the {lambda / cloudrun / cloudfunc /
//     azfunc / ocifunc} set.
//   - For each annotatable row, runs a SINGLE detection call
//     (one per-cloud invocation-count query + one in-memory
//     traceindex lookup); populates snapshots[i].SamplingRatio
//     and snapshots[i].SamplingExceedsFloor from the result.
//   - On error: LOGS a warning and continues. A flaky per-cloud
//     metric API degrades a row to nil pointers (rendered as "—"
//     in the UI) rather than failing the scan endpoint, matching
//     AnnotateServerlessWithColdStart posture.
//
// See docs/proposals/sampling-rate-analysis-slice1.md §6.2.

// SamplingAnnotator is the seam the annotator calls to compute
// the per-resource sampling-rate detection result at scan-response
// time. Production wires a closure holding the per-cloud
// MetricQuerier + the traceindex Quality observer.
//
// Returning a populated result + nil error is the success path;
// nil error + zero-value result means "no observation yet"
// (annotator leaves the row's pointers nil so the UI renders "—").
type SamplingAnnotator interface {
	AnnotateSampling(
		ctx context.Context,
		resourceARN string,
		surface string,
		traceindexKey string,
	) (proposer.SamplingRateDetectionResult, error)
}

// SamplingKeyResolver maps a (surface, resource ARN) tuple to the
// traceindex key the Quality observer uses for the resource.
// Production wires the same ComputeResourceKey path the OTLP
// receiver uses so the join lines up exactly; tests substitute a
// constant.
type SamplingKeyResolver interface {
	TraceindexKeyFor(surface, resourceARN string) string
}

// AnnotateServerlessWithSampling iterates the supplied serverless
// snapshots and populates SamplingRatio + SamplingExceedsFloor
// from the per-cloud sampling-rate detection. annotator nil OR
// resolver nil short-circuits the entire call (no-op). A flaky
// per-cloud metric API degrades a row to nil pointers rather than
// failing the scan endpoint.
//
// Surfaces in the slice 1 set: lambda / cloudrun / cloudfunc /
// azfunc / ocifunc. Unknown surfaces skip silently — the new
// fields stay nil so the UI renders "—" on rendered rows.
//
// ResourceARN is the join key — empty ResourceARN rows skip
// silently (degenerate inventory rows can't be joined).
//
// Insufficient-data semantics: when the result's
// ExpectedInvocationCount is 0 (no invocations in the window) OR
// the result's ObservedSpanCount is 0 AND
// ExpectedInvocationCount is below the minimum, the annotator
// leaves the row's pointers nil. The "had an observation but no
// invocations" case is indistinguishable from "no observation
// yet" at the UI level — both render as "—".
func AnnotateServerlessWithSampling(
	ctx context.Context,
	annotator SamplingAnnotator,
	resolver SamplingKeyResolver,
	snapshots []scanner.ServerlessInstanceSnapshot,
	logger *zap.Logger,
) {
	if annotator == nil || resolver == nil || len(snapshots) == 0 {
		return
	}
	for i := range snapshots {
		if !isSamplingAnnotatableSurface(snapshots[i].Surface) {
			continue
		}
		if snapshots[i].ResourceARN == "" {
			continue
		}
		arn := snapshots[i].ResourceARN
		key := resolver.TraceindexKeyFor(snapshots[i].Surface, arn)

		result, err := annotator.AnnotateSampling(ctx, arn, snapshots[i].Surface, key)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory sampling annotation failed",
					zap.String("resource_arn", arn),
					zap.String("surface", snapshots[i].Surface),
					zap.String("category", "serverless"),
					zap.Error(err))
			}
			continue
		}
		// "No data yet" — both observed and expected at 0. Leave
		// the row's pointers nil so the UI renders "—".
		if result.ExpectedInvocationCount == 0 && result.ObservedSpanCount == 0 {
			continue
		}

		ratio := result.Ratio
		snapshots[i].SamplingRatio = &ratio

		// would_fire predicate: combined (below floor AND above
		// minimum). The UI's amber-color logic reads this; the
		// chunk-3 tooltip surfaces the underlying counts so an
		// operator can see why the gate held / didn't.
		exceeds := result.ShouldFireRecommendation()
		snapshots[i].SamplingExceedsFloor = &exceeds
	}
}

// isSamplingAnnotatableSurface is the surface-discriminator gate
// the annotator uses. Slice 1 covers all 5 serverless surfaces.
// Unknown surfaces (future slice-2+ surfaces) skip silently so
// the annotator stays forward-compatible.
func isSamplingAnnotatableSurface(surface string) bool {
	switch surface {
	case "lambda", "cloudrun", "cloudfunc", "azfunc", "ocifunc":
		return true
	default:
		return false
	}
}
