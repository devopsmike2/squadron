// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/iac"
	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/recommendations"
)

// Regression-recommendation wiring (detection → proposal). The cold-start +
// error-rate REGRESSION detectors persist observations and annotate the
// serverless inventory rows (ColdStartExceedsThreshold etc.), but the
// deterministic recommendation builders in internal/proposer
// (CheckLambdaColdStartBatch + per-cloud / error-rate siblings) had no
// production caller — the detection dead-ended as a UI cell instead of
// becoming an actionable Terraform PR. This file closes that loop for the
// discovery recommendations flow.
//
// Slice 1 wires the AWS Lambda cold-start path (the reference). Slice 2
// extends to error-rate + the GCP/Azure/OCI surfaces, reusing this
// foundation. The recs only appear when a prior scan produced the detection
// annotations (i.e. the operator enabled the commercial-tier detectors for
// the add-on-backed clouds, or the signal is OSS-native) — so the surface is
// naturally gated by data availability, no extra flag.

// coldStartBaselineWindowHours / coldStartCurrentWindowHours are the
// observation windows the detector persists under; the recs path reads them
// back to enrich the reasoning text with the baseline + ratio numbers.
const (
	coldStartCurrentWindowHours  = 24
	coldStartBaselineWindowHours = 168
)

// appendAWSColdStartRegressionRecs appends one cold-start regression
// recommendation per AWS Lambda row whose canonical exceeds-threshold flag is
// set on the scan response. Best-effort: a nil store, missing observations, or
// a builder error never blocks the LLM recs already in `recs` — the regression
// recs are purely additive. Exclusions are honored inside the builder (the
// application store satisfies proposer.ColdStartExclusionStore).
func (h *DiscoveryHandlers) appendAWSColdStartRegressionRecs(
	ctx context.Context, recs *[]recommendations.Recommendation, scan awsScanResponse, now time.Time,
) {
	scope := proposer.ColdStartScope{
		ConnectionID: scan.AccountID, // AWS: connectionID == accountID
		ScopeID:      scan.AccountID,
		Region:       firstRegion(scan.Regions),
	}

	var rows []proposer.ColdStartInventoryRow
	var findings []*proposer.ColdStartDetectionFinding
	for _, sv := range scan.Serverless {
		if sv.Surface != "lambda" || sv.ResourceARN == "" {
			continue
		}
		// Gate on the canonical operator-facing predicate already computed by
		// the cold-start annotation pass — zero threshold drift.
		if sv.ColdStartExceedsThreshold == nil || !*sv.ColdStartExceedsThreshold {
			continue
		}
		finding := proposer.ColdStartDetectionFinding{ShouldFire: true}
		if sv.ColdStartP95Ms != nil {
			finding.CurrentP95Ms = *sv.ColdStartP95Ms
		}
		// Enrich baseline + ratio + sample counts from the observation store
		// for the reasoning text. Optional — absence just yields a terser
		// reasoning string, never a dropped recommendation.
		if h.coldStartStore != nil {
			if base, ok, err := h.coldStartStore.LatestColdStartObservation(ctx, sv.ResourceARN, coldStartBaselineWindowHours); err == nil && ok {
				finding.BaselineP95Ms = base.P95Ms
				finding.BaselineSampleCount = base.SampleCount
				if base.P95Ms > 0 {
					finding.Ratio = finding.CurrentP95Ms / base.P95Ms
				}
			}
			if cur, ok, err := h.coldStartStore.LatestColdStartObservation(ctx, sv.ResourceARN, coldStartCurrentWindowHours); err == nil && ok {
				finding.CurrentSampleCount = cur.SampleCount
			}
		}
		rows = append(rows, proposer.ColdStartInventoryRow{
			RecommendationID: sv.ResourceARN, // stable across scans → exclusions persist
			Provider:         "aws",
			Surface:          "lambda",
			ResourceTFName:   sv.ResourceName,
			ResourceID:       sv.ResourceARN,
			Region:           sv.Region,
		})
		findings = append(findings, &finding)
	}
	if len(rows) == 0 {
		return
	}

	drafts, errs := proposer.CheckLambdaColdStartBatch(ctx, rows, findings, scope, h.exclusionStore)
	for _, e := range errs {
		if e != nil && h.logger != nil {
			h.logger.Warn("aws cold-start regression rec build error", zap.Error(e))
		}
	}
	for _, d := range drafts {
		*recs = append(*recs, coldStartDraftToRecommendation(d, scan.ScanID, now))
	}
}

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
		ResourceKind:      d.Kind, // "lambda-cold-start-baseline"
		AffectedResources: []string{d.ResourceID},
	}
	if rec.ResourceKind != "" {
		rec.Disposition = iac.DispositionFor(rec.ResourceKind)
	}
	return rec
}
