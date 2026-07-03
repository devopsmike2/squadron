// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// inventory_error_rate.go — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Sibling of inventory_cold_start.go +
// inventory_sampling.go: per-provider scan handlers call
// AnnotateServerlessWithErrorRate AFTER the scanner produces a
// Result and AFTER the trace-emission LastSeenAt + cold-start +
// sampling annotations, BEFORE marshalScanResult serializes the
// response.
//
// The helper:
//   - Returns immediately when the supplied
//     ErrorRateObservationStore is nil (error-rate detection
//     disabled in this deployment, or the chunk-1 storage layer
//     isn't wired).
//   - Iterates the supplied serverless snapshots, skipping any
//     with surface not in {lambda / cloudrun / cloudfunc / azfunc
//     / ocifunc}.
//   - For each annotatable row, performs two cheap LatestErrorRate
//     Observation lookups (24h current + 168h baseline) and
//     populates snapshots[i].CurrentErrorRate +
//     snapshots[i].ErrorRateExceedsThreshold from the rows.
//   - On error: LOGS a warning and continues. A flaky storage
//     layer degrades a row to nil pointers (rendered as "—" in
//     the UI) rather than failing the scan endpoint, matching
//     AnnotateServerlessWithColdStart / Sampling posture.
//
// See docs/proposals/error-rate-correlation-slice1.md §6.2 +
// §11 acceptance tests 13-14.

// ErrorRateObservationStore is the slim interface the annotator
// reads. Satisfied by *sqlite.Storage's
// LatestErrorRateObservation method (added in chunk 1 / v0.89.127).
// Distinct from the cold-start / sampling stores so a test stub
// can mock just the error-rate slice without coupling the three
// branches.
type ErrorRateObservationStore interface {
	LatestErrorRateObservation(
		ctx context.Context,
		connectionID string,
		resourceARN string,
		windowHours int,
	) (sqlite.ErrorRateObservationRow, bool, error)
}

// AnnotateServerlessWithErrorRate iterates the supplied serverless
// snapshots and populates CurrentErrorRate +
// ErrorRateExceedsThreshold from the persisted
// error_rate_observation table. store nil short-circuits the
// entire call (no-op). A flaky storage layer degrades a row to
// nil pointers rather than failing the scan endpoint.
//
// Surfaces in the slice 1 set: lambda / cloudrun / cloudfunc /
// azfunc / ocifunc. Unknown surfaces skip silently.
//
// ResourceARN is the join key — empty ResourceARN rows skip
// silently (degenerate inventory rows can't be joined).
//
// The threshold predicate mirrors
// proposer.ErrorRateDetectionResult.ShouldFireRecommendation:
// true iff the current/baseline ratio exceeds
// proposer.ErrorRateRatioFloor (2.0) AND current invocations >=
// proposer.ErrorRateMinInvocationCount (1000) AND current errors
// >= proposer.ErrorRateMinErrorCount (50). The §12 baseline
// floor of 0.0001 (0.01%) is applied so a near-zero baseline
// doesn't produce a spurious large ratio.
//
// Insufficient-data semantics: when no current-window observation
// exists, the annotator leaves the row's pointers nil. When the
// current observation exists but the baseline does not (or has
// zero invocations), the threshold predicate degrades to false
// (the ratio can't be computed) but CurrentErrorRate is still
// populated so the absolute number renders.
func AnnotateServerlessWithErrorRate(
	ctx context.Context,
	store ErrorRateObservationStore,
	snapshots []scanner.ServerlessInstanceSnapshot,
	logger *zap.Logger,
) {
	if store == nil || len(snapshots) == 0 {
		return
	}
	for i := range snapshots {
		if !isErrorRateAnnotatableSurface(snapshots[i].Surface) {
			continue
		}
		if snapshots[i].ResourceARN == "" {
			continue
		}
		arn := snapshots[i].ResourceARN

		current, currentOK, err := store.LatestErrorRateObservation(
			ctx, "", arn, proposer.ErrorRateCurrentWindowHours,
		)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory error_rate current lookup failed",
					zap.String("resource_arn", arn),
					zap.String("surface", snapshots[i].Surface),
					zap.String("category", "serverless"),
					zap.Error(err))
			}
			continue
		}
		if !currentOK {
			// No observation yet — leave both fields nil so the
			// UI renders "—".
			continue
		}

		// Stamp the current-window error rate. Allocate a fresh
		// float64 so the omitempty pointer survives the JSON
		// marshal even when the value is 0.0 (a function that
		// genuinely had zero errors in the window — common for
		// healthy resources).
		rate := current.ErrorRate
		snapshots[i].CurrentErrorRate = &rate

		// Compute the threshold predicate from the current +
		// baseline rows. A baseline-lookup error stamps the
		// predicate as false (the cell renders un-colored) and
		// keeps CurrentErrorRate populated.
		baseline, baselineOK, err := store.LatestErrorRateObservation(
			ctx, "", arn, proposer.ErrorRateBaselineWindowHours,
		)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory error_rate baseline lookup failed",
					zap.String("resource_arn", arn),
					zap.Int("window_hours", proposer.ErrorRateBaselineWindowHours),
					zap.String("category", "serverless"),
					zap.Error(err))
			}
			no := false
			snapshots[i].ErrorRateExceedsThreshold = &no
			continue
		}

		exceeds := computeErrorRateExceedsThreshold(current, baseline, baselineOK)
		snapshots[i].ErrorRateExceedsThreshold = &exceeds
	}
}

// computeErrorRateExceedsThreshold mirrors
// proposer.ErrorRateDetectionResult.ShouldFireRecommendation
// against the persisted current + baseline rows. Pulled out as a
// named helper so the test suite can pin the predicate
// independently of the per-snapshot iteration.
//
// All three gates must hold:
//   - current/effective_baseline > 2.0 (the ratio gate, with the
//     §12 floor of 0.0001 applied when the raw baseline is below
//     the floor),
//   - current invocations >= 1000,
//   - current errors >= 50.
func computeErrorRateExceedsThreshold(
	current sqlite.ErrorRateObservationRow,
	baseline sqlite.ErrorRateObservationRow,
	baselineOK bool,
) bool {
	if uint64(current.InvocationCount) < proposer.ErrorRateMinInvocationCount {
		return false
	}
	if uint64(current.ErrorCount) < proposer.ErrorRateMinErrorCount {
		return false
	}
	effectiveBaseline := 0.0
	if baselineOK {
		effectiveBaseline = baseline.ErrorRate
	}
	if effectiveBaseline < proposer.ErrorRateBaselineFloor {
		effectiveBaseline = proposer.ErrorRateBaselineFloor
	}
	ratio := current.ErrorRate / effectiveBaseline
	return ratio > proposer.ErrorRateRatioFloor
}

// isErrorRateAnnotatableSurface is the surface-discriminator gate
// the annotator uses. Slice 1 covers all 5 serverless surfaces.
// Unknown surfaces skip silently so the annotator stays
// forward-compatible.
func isErrorRateAnnotatableSurface(surface string) bool {
	switch surface {
	case "lambda", "cloudrun", "cloudfunc", "azfunc", "ocifunc":
		return true
	default:
		return false
	}
}

// Compile-time interface check: *sqlite.Storage satisfies
// ErrorRateObservationStore directly via LatestErrorRateObservation
// from chunk 1. Re-asserted here so a future refactor to the
// storage signature surfaces at the error-rate handler boundary.
var _ ErrorRateObservationStore = (*sqlite.Storage)(nil)
