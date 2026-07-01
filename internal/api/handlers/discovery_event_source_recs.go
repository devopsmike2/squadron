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

// eventSourceRowsToInventory projects the snake_case event-source wire
// rows into the shared EventSourceInventoryRow the detection branches
// consume. Provider-agnostic: the row already carries Provider/Surface,
// and the two Detail-bag signals (has_reservation, has_capture) that
// aren't top-level axes are read out here. RecommendationID = ResourceARN
// so a decline persists across re-scans (matches the regression-recs
// stable-ID convention).
func eventSourceRowsToInventory(rows []eventSourceRow) []proposer.EventSourceInventoryRow {
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
			HasReservation:   detailBool(r.Detail, "has_reservation"),
			HasCapture:       detailBool(r.Detail, "has_capture"),
		})
	}
	return out
}

// detailBool reads a bool out of the scanned Detail bag, tolerating the
// key being absent (→ false) or JSON-decoded to a non-bool.
func detailBool(detail map[string]any, key string) bool {
	if detail == nil {
		return false
	}
	v, ok := detail[key].(bool)
	return ok && v
}

func (h *DiscoveryHandlers) appendAWSEventSourceRecs(
	ctx context.Context, recs *[]recommendations.Recommendation, scan awsScanResponse, now time.Time,
) {
	scope := proposer.EventSourceScope{
		ConnectionID: scan.AccountID, // AWS: connectionID == accountID
		ScopeID:      scan.AccountID,
		Region:       firstRegion(scan.Regions),
	}
	appendEventSourceRecs(ctx, recs, eventSourceRowsToInventory(scan.EventSources),
		h.exclusionStore, scope, scan.ScanID, now, h.logger)
}

// appendGCPEventSourceRecs folds the GCP event-source recommendations
// (Cloud Tasks retry/logging, Pub/Sub Lite logging/reservation) into the
// GCP recommendations flow. connID/projectID/region come from the
// connection; the exclusion scope mirrors the regression-recs scope
// (ConnectionID=conn.ID, ScopeID=projectID).
func (h *DiscoveryGCPHandlers) appendGCPEventSourceRecs(
	ctx context.Context, recs *[]recommendations.Recommendation,
	rows []eventSourceRow, connID, projectID, region, scanID string, now time.Time,
) {
	scope := proposer.EventSourceScope{ConnectionID: connID, ScopeID: projectID, Region: region}
	appendEventSourceRecs(ctx, recs, eventSourceRowsToInventory(rows),
		h.exclusionStore, scope, scanID, now, h.logger)
}

// appendAzureEventSourceRecs folds the Azure event-source
// recommendations (Event Grid diagnostics/CloudEvents-schema, Event Hubs
// diagnostics/Capture) into the Azure recommendations flow. The
// exclusion scope mirrors the regression-recs scope
// (ConnectionID=conn.ID, ScopeID=subscriptionID).
func (h *DiscoveryAzureHandlers) appendAzureEventSourceRecs(
	ctx context.Context, recs *[]recommendations.Recommendation,
	rows []eventSourceRow, connID, subscriptionID, region, scanID string, now time.Time,
) {
	scope := proposer.EventSourceScope{ConnectionID: connID, ScopeID: subscriptionID, Region: region}
	appendEventSourceRecs(ctx, recs, eventSourceRowsToInventory(rows),
		h.exclusionStore, scope, scanID, now, h.logger)
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
	case proposer.CloudTasksRetryPolicyRecommendationKind:
		return "Cloud Tasks queue has no retry policy"
	case proposer.CloudTasksLoggingRecommendationKind:
		return "Cloud Tasks queue logging not enabled"
	case proposer.PubSubLiteLoggingRecommendationKind:
		return "Pub/Sub Lite topic logging not enabled"
	case proposer.PubSubLiteReservationRecommendationKind:
		return "Pub/Sub Lite topic has no throughput reservation"
	case proposer.EventGridDiagnosticsRecommendationKind:
		return "Event Grid topic has no diagnostic setting"
	case proposer.EventGridCloudEventRecommendationKind:
		return "Event Grid topic not using the CloudEvents schema"
	case proposer.EventHubsDiagnosticsRecommendationKind:
		return "Event Hubs namespace has no diagnostic setting"
	case proposer.EventHubsCaptureRecommendationKind:
		return "Event Hubs namespace has no hub with Capture enabled"
	default:
		return "Event source observability gap"
	}
}
