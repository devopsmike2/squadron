// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"

	"github.com/devopsmike2/squadron/internal/proposer/iacpicker"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// event_source.go — event-source-tier recommendation branch. Sibling of
// error_rate.go / cold_start.go: a pure detection branch the discovery
// recommendations flow runs ALONGSIDE the LLM-proposed steps. For each
// scanned event-source row whose config-gap predicate fires, the branch
// emits a deterministic recommendation whose Terraform comes from the
// matching iacpicker.PickXxx pattern (built + tested in the
// event-source-tier slices but, until this reference slice, never
// reachable in production — the pickers were the finished last mile
// waiting on a detection branch to call them).
//
// Slice 1 of the picker-activation arc wires the AWS SNS
// delivery-logging kind. The scanned EventSourceInstanceSnapshot
// already carries HasLogAxis (aws/sns.go: true iff ANY per-protocol
// delivery-feedback role ARN is set on the topic), so the firing
// condition is a direct read — no new scanner work. Subsequent slices
// extend the same shape to SQS redrive / Cloud Tasks / Event Grid /
// Event Hubs / ONS / Pub/Sub Lite / Resource Manager.

// SNSDeliveryLoggingRecommendationKind — the recommendation kind the
// SNS delivery-logging branch emits. Matches the iacpicker doc + the
// webhook `sns-` prefix routing (providerFromRecommendationKind → aws)
// and the placement/disposition map entries.
const SNSDeliveryLoggingRecommendationKind = "sns-delivery-logging-enable"

// awsSNSSurface is the Surface discriminator the AWS scanner stamps on
// SNS-topic event-source snapshots (mirrors aws.SNSSurface without
// importing the scanner package into the picker branch — the two must
// stay in lockstep; the CheckSNSDeliveryLogging test pins the literal).
const awsSNSSurface = "sns"

// EventSourceInventoryRow is the minimal projection of a scanned
// event-source row the event-source recommendation branch reads. The
// handler builds it from the marshaled eventSourceRow / snapshot.
type EventSourceInventoryRow struct {
	// RecommendationID is the stable-across-scans identifier the
	// exclusion machinery keys on (the handler passes ResourceARN so a
	// decline persists across re-scans, matching the regression recs).
	RecommendationID string
	Provider         string // "aws" / "gcp" / "azure" / "oci"
	Surface          string // "sns" / "sqs" / "eventbridge" / ...
	// ResourceTFName is the best-effort Terraform resource name; the
	// picker falls back to "<name>" when empty.
	ResourceTFName string
	ResourceID     string // canonical ARN / resource path (AffectedResources)
	Region         string
	// HasLogAxis mirrors the scanned snapshot axis: true when a
	// structured-logging / delivery-audit destination is already wired.
	// The delivery-logging recommendation fires only when this is false.
	HasLogAxis bool
}

// EventSourceScope carries the connection identity the exclusion lookup
// needs (mirrors ErrorRateScope / ColdStartScope).
type EventSourceScope struct {
	ConnectionID string
	ScopeID      string
	Region       string
}

// EventSourceExclusionStore is the slim slice of the discovery
// exclusion store the branch needs — structurally satisfied by the app
// store, same as the regression branches.
type EventSourceExclusionStore interface {
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// EventSourceRecommendationDraft is the branch's output; the handler
// maps it onto the wire recommendation envelope (mirrors
// ErrorRateRecommendationDraft).
type EventSourceRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
}

// CheckSNSDeliveryLogging is the detection branch for the AWS SNS
// delivery-logging kind. It fires when the row is an SNS topic whose
// HasLogAxis is false (no per-protocol delivery-feedback role wired →
// CloudWatch is not recording per-message delivery success/failure) and
// the recommendation hasn't been excluded for this scope. The Terraform
// comes from iacpicker.PickSNSDeliveryLoggingPattern.
//
// Honest-framing parity with the picker's reasoning: a topic may
// intentionally route delivery audit to a non-CloudWatch destination,
// so the recommendation is declinable and the verdict-learning loop
// records the decline (via the exclusion store checked here).
//
// Returns (nil, nil) when the gate isn't met or the recommendation is
// excluded — additive + best-effort, never blocking the LLM recs.
func CheckSNSDeliveryLogging(
	ctx context.Context,
	row EventSourceInventoryRow,
	scope EventSourceScope,
	exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != awsSNSSurface {
		return nil, nil
	}
	// The topic already has a delivery-feedback destination wired —
	// nothing to recommend.
	if row.HasLogAxis {
		return nil, nil
	}

	recID := row.RecommendationID
	if recID == "" {
		recID = row.ResourceID
	}

	if exclusions != nil && scope.ConnectionID != "" && scope.ScopeID != "" {
		excluded, err := exclusions.ListExcludedRecommendations(
			ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
		)
		if err != nil {
			return nil, fmt.Errorf("sns delivery logging: list excluded recommendations: %w", err)
		}
		for _, ex := range excluded {
			if ex.RecommendationID != "" && ex.RecommendationID == recID {
				return nil, nil
			}
			if ex.RecommendationID == "" && ex.RecommendationKind == SNSDeliveryLoggingRecommendationKind {
				return nil, nil
			}
		}
	}

	terraform, reasoning := iacpicker.PickSNSDeliveryLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "aws",
		ResourceTFName: row.ResourceTFName,
	})

	return &EventSourceRecommendationDraft{
		Kind:             SNSDeliveryLoggingRecommendationKind,
		RecommendationID: recID,
		Reasoning:        reasoning,
		Terraform:        terraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
	}, nil
}
