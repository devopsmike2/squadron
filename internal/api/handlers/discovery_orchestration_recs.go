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

// Orchestration-tier recommendation wiring (detection → proposal). The OCI
// Resource Manager logging picker (iacpicker.PickResourceManagerLoggingPattern)
// was built + tested in the orchestration-tier slices and the LLM path
// references resmgr-logging-enable in the OCI system prompt, but the
// deterministic picker never emitted a recommendation — its detection branch
// (proposer.CheckResourceManagerLogging) had no production caller. #328 Slice 1
// taught the OCI scan handler to invoke ScanOrchestrations so RM Stacks reach
// the wire; this file (Slice 2) closes the loop, mirroring the event-source
// wiring in discovery_event_source_recs.go.
//
// An RM Stack scanned with HasLogAxis=false (no Logging log in the Stack's
// compartment carrying source service "resourcemanager") yields a
// resmgr-logging-enable recommendation whose Terraform is the picker's
// log-group + SERVICE-log pattern. Additive + best-effort: it appends to the
// LLM recs and is gated by data availability (only RM Stacks lacking the log
// axis fire) + exclusions.

// appendOrchestrationRecs appends one recommendation per orchestration row
// whose config-gap predicate fires. Best-effort + additive: a builder error or
// excluded row never blocks recs already in `recs`.
func appendOrchestrationRecs(
	ctx context.Context,
	recs *[]recommendations.Recommendation,
	rows []proposer.OrchestrationInventoryRow,
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
		draft, err := proposer.CheckResourceManagerLogging(ctx, row, scope, exclusions)
		if err != nil {
			if logger != nil {
				logger.Warn("orchestration rec build error", zap.Error(err), zap.String("surface", row.Surface))
			}
			continue
		}
		if draft == nil {
			continue // gate not met or excluded.
		}
		*recs = append(*recs, orchestrationDraftToRecommendation(*draft, scanID, now))
	}
}

// orchestrationRowsToInventory projects the shared awsOrchestrationRow wire
// rows into the OrchestrationInventoryRow the detection branch consumes.
// RecommendationID = ResourceARN so a decline persists across re-scans (matches
// the event-source + regression-recs stable-ID convention).
func orchestrationRowsToInventory(rows []awsOrchestrationRow) []proposer.OrchestrationInventoryRow {
	out := make([]proposer.OrchestrationInventoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, proposer.OrchestrationInventoryRow{
			RecommendationID: r.ResourceARN,
			Provider:         r.Provider,
			Surface:          r.Surface,
			ResourceTFName:   r.ResourceName,
			ResourceID:       r.ResourceARN,
			Region:           r.Region,
			HasLogAxis:       r.HasLogAxis,
		})
	}
	return out
}

// appendOCIOrchestrationRecs folds the OCI orchestration recommendations
// (Resource Manager logging) into the OCI recommendations flow. The exclusion
// scope mirrors the event-source + regression-recs scope (ConnectionID=conn.ID,
// ScopeID=tenancyOCID).
func (h *DiscoveryOCIHandlers) appendOCIOrchestrationRecs(
	ctx context.Context, recs *[]recommendations.Recommendation,
	rows []awsOrchestrationRow, connID, tenancyOCID, region, scanID string, now time.Time,
) {
	scope := proposer.EventSourceScope{ConnectionID: connID, ScopeID: tenancyOCID, Region: region}
	appendOrchestrationRecs(ctx, recs, orchestrationRowsToInventory(rows),
		h.exclusionStore, scope, scanID, now, h.logger)
}

// orchestrationDraftToRecommendation maps an OrchestrationRecommendationDraft
// onto the wire recommendation envelope (mirrors eventSourceDraftToRecommendation)
// so the rec renders + opens a PR exactly like an LLM-proposed step.
func orchestrationDraftToRecommendation(
	d proposer.OrchestrationRecommendationDraft, scanID string, now time.Time,
) recommendations.Recommendation {
	rec := recommendations.Recommendation{
		ID:          d.RecommendationID,
		Category:    recommendations.CategoryEmptySignal,
		Severity:    recommendations.SeverityWarn,
		Title:       orchestrationRecTitle(d.Kind),
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
		ResourceKind:      d.Kind, // "resmgr-logging-enable"
		AffectedResources: []string{d.ResourceID},
	}
	if rec.ResourceKind != "" {
		rec.Disposition = iac.DispositionFor(rec.ResourceKind)
	}
	return rec
}

// orchestrationRecTitle maps an orchestration recommendation kind to its
// operator-facing card title. Unknown kinds fall back to a generic
// orchestration title so a newly-added kind never renders blank.
func orchestrationRecTitle(kind string) string {
	switch kind {
	case proposer.ResourceManagerLoggingRecommendationKind:
		return "OCI Resource Manager Stack logging not enabled"
	default:
		return "Orchestration observability gap"
	}
}
