// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"

	"github.com/devopsmike2/squadron/internal/proposer/iacpicker"
)

// orchestration.go — orchestration-tier recommendation branch. Sibling of
// event_source.go: a pure detection branch the discovery recommendations flow
// runs ALONGSIDE the LLM-proposed steps. For each scanned orchestration row
// whose config-gap predicate fires, the branch emits a deterministic
// recommendation whose Terraform comes from the matching iacpicker.PickXxx
// pattern.
//
// The picker (iacpicker.PickResourceManagerLoggingPattern) and the OCI
// Resource Manager scanner (ScanResourceManagerStacks, which stamps
// HasLogAxis=false when no Logging log in the Stack's compartment carries
// source service "resourcemanager") were both built + tested in the
// orchestration-tier slices, but the detection branch that turns a scanned RM
// Stack into a recommendation was never written — so the picker was the
// finished last mile with no caller. This closes that loop, mirroring the
// event-source picker-activation arc one kind at a time.
//
// Scope + exclusion machinery is shared with event_source.go (EventSourceScope
// / EventSourceExclusionStore / eventSourceExcluded are generic connection-
// scoping types, not SNS-specific), so this file adds only the orchestration-
// specific kind, row projection, draft, and Check.

// ResourceManagerLoggingRecommendationKind is the single orchestration-tier
// kind the branch emits. Matches the picker doc, the webhook prefix routing
// (providerFromRecommendationKind → oci), the proposer prompt block, and its
// placement/disposition map entries.
const ResourceManagerLoggingRecommendationKind = "resmgr-logging-enable"

// ociResourceManagerSurface is the Surface discriminator the OCI scanner
// stamps on Resource Manager Stack snapshots (scanner_resmgr.go
// OCIResourceManagerSurface). Mirrored here so the detection branch stays
// dependency-light; the Check test pins the literal against the scanner const.
const ociResourceManagerSurface = "resmgr"

// OrchestrationInventoryRow is the minimal projection of a scanned
// orchestration row the branch reads. The handler builds it from the marshaled
// awsOrchestrationRow / snapshot.
type OrchestrationInventoryRow struct {
	// RecommendationID is the stable-across-scans identifier the exclusion
	// machinery keys on (the handler passes ResourceARN so a decline persists
	// across re-scans, matching the event-source + regression recs).
	RecommendationID string
	Provider         string // "oci" (only OCI RM ships an orchestration picker today)
	Surface          string // "resmgr"
	// ResourceTFName is the best-effort Terraform resource name; the picker
	// falls back to "<name>" when empty.
	ResourceTFName string
	ResourceID     string // canonical OCID (AffectedResources)
	Region         string
	// HasLogAxis mirrors the scanned snapshot axis: true when a Logging log
	// with source service "resourcemanager" already exists in the Stack's
	// compartment. The logging-enable recommendation fires only when false.
	HasLogAxis bool
}

// OrchestrationRecommendationDraft is the branch's output; the handler maps it
// onto the wire recommendation envelope (mirrors EventSourceRecommendationDraft).
type OrchestrationRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
}

// CheckResourceManagerLogging is the detection branch for the OCI Resource
// Manager logging kind. It fires when the row is an RM Stack whose HasLogAxis
// is false (no RM-source Logging in the Stack's compartment → Job apply/destroy
// events leave no audit trail beyond the OCI console) and the recommendation
// hasn't been excluded for this scope. The Terraform comes from
// iacpicker.PickResourceManagerLoggingPattern.
//
// Honest-framing parity with the picker's reasoning: a team may intentionally
// route Stack audit to a non-OCI-Logging destination, so the recommendation is
// declinable and the verdict-learning loop records the decline (via the
// exclusion store checked here). Reuses EventSourceScope + the generic
// eventSourceExcluded helper — the exclusion keys are connection-scoped, not
// tier-specific.
//
// Returns (nil, nil) when the gate isn't met or the recommendation is excluded
// — additive + best-effort, never blocking the LLM recs.
func CheckResourceManagerLogging(
	ctx context.Context,
	row OrchestrationInventoryRow,
	scope EventSourceScope,
	exclusions EventSourceExclusionStore,
) (*OrchestrationRecommendationDraft, error) {
	if row.Surface != ociResourceManagerSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := row.RecommendationID
	if recID == "" {
		recID = row.ResourceID
	}
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, ResourceManagerLoggingRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickResourceManagerLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "oci",
		ResourceTFName: row.ResourceTFName,
	})
	return &OrchestrationRecommendationDraft{
		Kind:             ResourceManagerLoggingRecommendationKind,
		RecommendationID: recID,
		Reasoning:        reasoning,
		Terraform:        terraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
	}, nil
}
