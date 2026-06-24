// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// inventory_cold_start.go — Cold-start latency analysis slice 1 chunk
// 3 (v0.89.115, #753 Stream 151). Sibling of inventory_lastseen.go:
// per-provider scan handlers call AnnotateServerlessWithColdStart
// AFTER the scanner produces a Result and AFTER the trace-emission
// LastSeenAt annotation, BEFORE marshalScanResult serializes the
// response. The helper:
//   - Returns immediately when the supplied ColdStartObservationReader
//     is nil (cold-start detection disabled in this deployment, or the
//     chunk-1 storage layer isn't wired).
//   - Iterates the supplied serverless snapshots, skipping any with
//     surface != "lambda" (slice 1 AWS-only).
//   - For each Lambda row, runs a SINGLE LatestColdStartObservation
//     call for the 24-hour current window; populates
//     snapshots[i].ColdStartP95Ms from the row's P95Ms.
//   - Computes ColdStartExceedsThreshold by ALSO running a baseline-
//     window LatestColdStartObservation lookup and applying the
//     substrate-default 1.5x ratio + 500ms floor predicates. Two
//     window lookups per Lambda is the same pattern the chunk-2
//     per-resource cold_start endpoint uses (HandleColdStart).
//   - On error: LOGS a warning and continues. A flaky storage layer
//     degrades a row to nil pointers (rendered as "—" in the UI)
//     rather than failing the scan endpoint, matching the
//     AnnotateComputeWithLastSeen posture.
//
// Per-Lambda detection result lookup: the helper queries the
// cold_start_observation table at request time. The chunk-2 scanner
// already persisted the observations during the scan (
// runColdStartDetectionForServerless) — this handler does NOT re-run
// CloudWatch. Two cheap SELECT statements per Lambda is acceptable for
// slice 1 fleet sizes; slice 2 may add a per-scan cached field on
// ServerlessInstanceSnapshot to elide the round-trip when the scan
// caller is the same process. The current shape keeps the storage
// query at the handler boundary so an out-of-band caller (e.g. the UI
// re-rendering an existing scan) sees the latest observation without
// re-scanning.
//
// See docs/proposals/cold-start-latency-slice1.md §6.2 + §7 + §11
// acceptance tests 12-14.

// ColdStartAnnotationThresholds is the slim slice of
// ColdStartDetectionConstants the annotator reads. v0.89.115 — same
// interface seam the chunk-2 per-resource handler uses so the four
// substrate constants (current/baseline window hours + ratio + floor)
// stay single-sourced from internal/discovery/aws.
type ColdStartAnnotationThresholds interface {
	CurrentWindowHours() int
	BaselineWindowHours() int
	RatioThreshold() float64
	FloorMs() float64
}

// AnnotateServerlessWithColdStart iterates the supplied serverless
// snapshots and populates ColdStartP95Ms + ColdStartExceedsThreshold
// from the persisted cold_start_observation table. lookup nil OR
// thresholds nil short-circuits the entire call (no-op). A flaky
// storage layer degrades a row to nil pointers rather than failing
// the scan endpoint.
//
// Slice 1 AWS Lambda only — rows with surface != "lambda" are skipped
// silently, leaving the two fields nil so the UI renders the canonical
// "—" surface on non-AWS serverless tables.
//
// Resource ARN is the join key against the cold_start_observation
// table — the scanner persists the same string in the resource_arn
// column at scan time. Empty ResourceARN rows skip silently (degenerate
// inventory rows can't be joined).
func AnnotateServerlessWithColdStart(
	ctx context.Context,
	lookup ColdStartObservationReader,
	thresholds ColdStartAnnotationThresholds,
	snapshots []scanner.ServerlessInstanceSnapshot,
	logger *zap.Logger,
) {
	if lookup == nil || thresholds == nil || len(snapshots) == 0 {
		return
	}
	currentHours := thresholds.CurrentWindowHours()
	baselineHours := thresholds.BaselineWindowHours()
	ratio := thresholds.RatioThreshold()
	floorMs := thresholds.FloorMs()

	for i := range snapshots {
		// Slice 1 AWS Lambda only — leave nil pointers on
		// non-Lambda surfaces so the per-cloud Serverless tables on
		// GCP / Azure / OCI render the canonical "—" everywhere.
		if snapshots[i].Surface != "lambda" {
			continue
		}
		if snapshots[i].ResourceARN == "" {
			continue
		}
		arn := snapshots[i].ResourceARN

		current, currentOK, err := lookup.LatestColdStartObservation(ctx, arn, currentHours)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory cold_start lookup failed",
					zap.String("resource_arn", arn),
					zap.Int("window_hours", currentHours),
					zap.String("category", "serverless"),
					zap.Error(err))
			}
			continue
		}
		if !currentOK {
			// No observation yet for the current window — leave
			// both fields nil so the UI renders "—".
			continue
		}

		// Stamp the current-window P95 on the row. Allocate a fresh
		// float64 so the omitempty pointer survives the JSON marshal
		// even when the value is 0.0 (Lambda that genuinely had a
		// 0ms init in the window — possible for native-binary
		// functions). Storing a nil pointer would elide the field
		// entirely, which is the wrong surface here.
		p95 := current.P95Ms
		snapshots[i].ColdStartP95Ms = &p95

		// Threshold predicate: matches the chunk-2 per-resource
		// HandleColdStart response so the UI keeps a single
		// definition of "amber" across both surfaces. Baseline
		// lookup may legitimately return ok=false on Lambdas
		// younger than 7 days — in that case the threshold can't
		// trip, so we stamp ExceedsThreshold=false and let the UI
		// render the cell at the default color.
		baseline, baselineOK, err := lookup.LatestColdStartObservation(ctx, arn, baselineHours)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory cold_start baseline lookup failed",
					zap.String("resource_arn", arn),
					zap.Int("window_hours", baselineHours),
					zap.String("category", "serverless"),
					zap.Error(err))
			}
			// Stamp the threshold predicate as false so the cell
			// renders un-colored; the current-window P95 still
			// shows so the operator sees the absolute number.
			no := false
			snapshots[i].ColdStartExceedsThreshold = &no
			continue
		}
		exceeds := false
		if baselineOK && baseline.P95Ms > 0 {
			r := current.P95Ms / baseline.P95Ms
			exceeds = r >= ratio && current.P95Ms >= floorMs
		}
		snapshots[i].ColdStartExceedsThreshold = &exceeds
	}
}

// Compile-time interface check: *sqlite.Storage satisfies
// ColdStartObservationReader directly via LatestColdStartObservation
// from chunk 1. Re-asserted here so a future refactor to the storage
// signature surfaces at the cold-start handler boundary.
var _ ColdStartObservationReader = (*sqlite.Storage)(nil)
