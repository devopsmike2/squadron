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

// Event-source-tier recommendation wiring (detection → proposal). The
// event-source pickers in internal/proposer/iacpicker (PickSNSDelivery
// LoggingPattern et al.) were built + tested in the event-source-tier
// slices but had no production caller — the scanner surfaced the
// event-source rows (with the HasLogAxis config-gap axis) and the LLM
// path referenced them in the prompt, but the deterministic pickers
// never emitted a recommendation. This file closes that loop, mirroring
// the regression-recs wiring in discovery_regression_recs.go.
//
// Slice 1 wires the AWS SNS delivery-logging kind: an SNS topic scanned
// with HasLogAxis=false (no per-protocol delivery-feedback role) yields
// an sns-delivery-logging-enable recommendation whose Terraform is the
// picker's IAM-role + feedback-attachment pattern. Additive + best
// effort: it appends to the LLM recs and is gated by data availability
// (only SNS rows lacking the log axis fire) + exclusions.

// appendEventSourceRecs appends one recommendation per event-source row
// whose config-gap predicate fires. Cloud-agnostic over the shared
// EventSourceInventoryRow projection; the per-cloud adapters below feed
// it. Best-effort + additive: a builder error or excluded row never
// blocks recs already in `recs`.
func appendEventSourceRecs(
	ctx context.Context,
	recs *[]recommendations.Recommendation,
	rows []proposer.EventSourceInventoryRow,
	exclusions proposer.EventSourceExclusionStore,
	scope proposer.EventSourceScope,
	scanID string,
	now time.Time,
	logger *zap.Logger,
) {
	for _, row := range rows {
		if row.ResourceID == "" {
			continue
		}
		// Run every registered detection branch over the row. Each check
		// self-gates on Surface + its config axis, so a row can produce
		// zero, one, or (for multi-signal surfaces like Cloud Tasks) more
		// than one recommendation.
		for _, check := range proposer.EventSourceChecks {
			draft, err := check(ctx, row, scope, exclusions)
			if err != nil {
				if logger != nil {
					logger.Warn("event-source rec build error", zap.Error(err), zap.String("surface", row.Surface))
				}
				continue
			}
			if draft == nil {
				continue // gate not met or excluded.
			}
			*recs = append(*recs, eventSourceDraftToRecommendation(*draft, scanID, now))
		}
	}
}

// awsEventSourceRowsToInventory projects the AWS snake_case event-source
// wire rows into the shared EventSourceInventoryRow the branch consumes.
// RecommendationID = ResourceARN so a decline persists across re-scans
// (matches the regression-recs stable-ID convention).
func awsEventSourceRowsToInventory(rows []eventSourceRow) []proposer.EventSourceInventoryRow {
	out := make([]proposer.EventSourceInventoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, proposer.EventSourceInventoryRow{
			RecommendationID: r.ResourceARN,
			Provider:         r.Provider,
			Surface:          r.Surface,
			ResourceTFName:   r.ResourceName,
			ResourceID:       r.ResourceARN,
			Region:           r.Region,
			HasLogAxis:       r.HasLogAxis,
			HasTraceAxis:     r.HasTraceAxis,
		})
	}
	return out
}

func (h *DiscoveryHandlers) appendAWSEventSourceRecs(
	ctx context.Context, recs *[]recommendations.Recommendation, scan awsScanResponse, now time.Time,
) {
	scope := proposer.EventSourceScope{
		ConnectionID: scan.AccountID, // AWS: connectionID == accountID
		ScopeID:      scan.AccountID,
		Region:       firstRegion(scan.Regions),
	}
	appendEventSourceRecs(ctx, recs, awsEventSourceRowsToInventory(scan.EventSources),
		h.exclusionStore, scope, scan.ScanID, now, h.logger)
}

// eventSourceDraftToRecommendation maps an EventSourceRecommendationDraft
// onto the wire recommendation envelope (mirrors
// coldStartDraftToRecommendation) so the rec renders + opens a PR
// exactly like an LLM-proposed step.
func eventSourceDraftToRecommendation(
	d proposer.EventSourceRecommendationDraft, scanID string, now time.Time,
) recommendations.Recommendation {
	rec := recommendations.Recommendation{
		ID:          d.RecommendationID,
		Category:    recommendations.CategoryEmptySignal,
		Severity:    recommendations.SeverityWarn,
		Title:       eventSourceRecTitle(d.Kind),
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
		ResourceKind:      d.Kind, // "sns-delivery-logging-enable"
		AffectedResources: []string{d.ResourceID},
	}
	if rec.ResourceKind != "" {
		rec.Disposition = iac.DispositionFor(rec.ResourceKind)
	}
	return rec
}

// eventSourceRecTitle maps an event-source recommendation kind to its
// operator-facing card title. Unknown kinds fall back to a generic
// event-source title so a newly-added kind never renders blank.
func eventSourceRecTitle(kind string) string {
	switch kind {
	case proposer.SNSDeliveryLoggingRecommendationKind:
		return "SNS delivery-status logging not enabled"
	case proposer.SQSRedrivePolicyRecommendationKind:
		return "SQS queue has no redrive policy (silent message loss)"
	default:
		return "Event source observability gap"
	}
}
