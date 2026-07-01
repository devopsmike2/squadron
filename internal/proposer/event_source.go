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

// Recommendation kinds the event-source detection branches emit. Each
// matches its iacpicker doc, its webhook prefix routing
// (providerFromRecommendationKind), and its placement/disposition map
// entries.
const (
	SNSDeliveryLoggingRecommendationKind    = "sns-delivery-logging-enable"
	SQSRedrivePolicyRecommendationKind      = "sqs-redrive-policy-enable"
	CloudTasksRetryPolicyRecommendationKind = "cloudtasks-retry-policy-enable"
	CloudTasksLoggingRecommendationKind     = "cloudtasks-logging-enable"
	PubSubLiteLoggingRecommendationKind     = "pubsublite-logging-enable"
	PubSubLiteReservationRecommendationKind = "pubsublite-reservation-attach"
	EventGridDiagnosticsRecommendationKind  = "eventgrid-diagnostics-enable"
	EventGridCloudEventRecommendationKind   = "eventgrid-cloudevent-schema-enforce"
	EventHubsDiagnosticsRecommendationKind  = "eventhubs-diagnostics-enable"
	EventHubsCaptureRecommendationKind      = "eventhubs-capture-enable"
	ONSLoggingRecommendationKind            = "ons-logging-enable"
	QueuesLoggingRecommendationKind         = "queues-logging-enable"
)

// Surface discriminators the scanners stamp on event-source snapshots.
// Mirrored here (rather than importing each cloud's scanner package)
// so the detection branch stays dependency-light; the per-Check tests
// pin the literals against the scanner constants.
const (
	awsSNSSurface           = "sns"
	awsSQSSurface           = "sqs"
	gcpCloudTasksSurface    = "cloudtasks"
	gcpPubSubLiteSurface    = "pubsublite"
	azureEventGridSurface   = "eventgrid"
	azureEventHubsSurface   = "eventhubs"
	ociNotificationsSurface = "notifications"
	ociQueuesSurface        = "queues"
)

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
	// HasTraceAxis mirrors the scanned snapshot's trace primitive axis.
	// Per-surface meaning: SQS → a redrive policy (→ DLQ) is configured;
	// Event Grid → the topic uses the CloudEvents schema; Cloud Tasks →
	// a retry policy is set. The corresponding recommendation fires when
	// this is false.
	HasTraceAxis bool
	// HasReservation / HasCapture carry the two per-surface Detail-bag
	// signals that aren't top-level axes: GCP Pub/Sub Lite reservation
	// attachment (Detail["has_reservation"]) and Azure Event Hubs
	// Capture (Detail["has_capture"]). The per-cloud row→inventory
	// projection reads them out of the Detail map. False → the
	// corresponding recommendation fires.
	HasReservation bool
	HasCapture     bool
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

// EventSourceCheck is a single event-source detection branch: it
// self-gates on the row's Surface + the relevant config axis and returns
// a draft when the recommendation should fire (nil otherwise). The
// handler runs every registered check over every scanned row.
type EventSourceCheck func(
	ctx context.Context,
	row EventSourceInventoryRow,
	scope EventSourceScope,
	exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error)

// EventSourceChecks is the registry of active event-source detection
// branches. Adding a picker to production is a one-line append here (plus
// the Check func + its placement/disposition map entries). Each check is
// surface-gated, so running all of them over every row is safe: a check
// returns (nil, nil) for rows it doesn't own.
var EventSourceChecks = []EventSourceCheck{
	CheckSNSDeliveryLogging,
	CheckSQSRedrive,
	CheckCloudTasksRetryPolicy,
	CheckCloudTasksLogging,
	CheckPubSubLiteLogging,
	CheckPubSubLiteReservation,
	CheckEventGridDiagnostics,
	CheckEventGridCloudEventSchema,
	CheckEventHubsDiagnostics,
	CheckEventHubsCapture,
	CheckONSLogging,
	CheckQueuesLogging,
}

// resolveRecID picks the stable recommendation identifier for a row
// (RecommendationID, falling back to the canonical ResourceID).
func resolveRecID(row EventSourceInventoryRow) string {
	if row.RecommendationID != "" {
		return row.RecommendationID
	}
	return row.ResourceID
}

// eventSourceExcluded reports whether a recommendation (by ID or by
// kind-level exclusion) has been declined for this scope. A nil store or
// an incomplete scope means "not excluded" (the caller still fires — the
// exclusion check is best-effort). The error is surfaced so a store
// failure doesn't silently suppress the check.
func eventSourceExcluded(
	ctx context.Context,
	exclusions EventSourceExclusionStore,
	scope EventSourceScope,
	recID, kind string,
) (bool, error) {
	if exclusions == nil || scope.ConnectionID == "" || scope.ScopeID == "" {
		return false, nil
	}
	excluded, err := exclusions.ListExcludedRecommendations(
		ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
	)
	if err != nil {
		return false, fmt.Errorf("event source (%s): list excluded recommendations: %w", kind, err)
	}
	for _, ex := range excluded {
		if ex.RecommendationID != "" && ex.RecommendationID == recID {
			return true, nil
		}
		if ex.RecommendationID == "" && ex.RecommendationKind == kind {
			return true, nil
		}
	}
	return false, nil
}

// buildEventSourceDraft assembles the draft from a (terraform, reasoning)
// picker result. Kept tiny so each Check reads as gate → picker → draft.
func buildEventSourceDraft(
	kind, recID string, row EventSourceInventoryRow, scope EventSourceScope,
	terraform, reasoning string,
) *EventSourceRecommendationDraft {
	return &EventSourceRecommendationDraft{
		Kind:             kind,
		RecommendationID: recID,
		Reasoning:        reasoning,
		Terraform:        terraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
	}
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
	if row.Surface != awsSNSSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, SNSDeliveryLoggingRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickSNSDeliveryLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "aws",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(SNSDeliveryLoggingRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckSQSRedrive is the detection branch for the AWS SQS
// redrive-policy kind. It fires when the row is an SQS queue whose
// HasTraceAxis is false — i.e. NO redrive policy is configured, so
// failed messages vanish silently once the retention window expires
// (the single most common AWS messaging production failure, per
// event-source-tier-slice4 §3). The Terraform comes from
// iacpicker.PickSQSRedrivePolicyPattern.
//
// Note the axis: HasTraceAxis (redrive policy present) is the fire gate,
// NOT HasLogAxis (DLQ reachable). A queue WITH a redrive policy but an
// unreachable cross-account DLQ (HasTraceAxis true, HasLogAxis false) is
// the audit-only sqs-deadletter-queue-attach case, which carries no
// Terraform and is intentionally not emitted here.
func CheckSQSRedrive(
	ctx context.Context,
	row EventSourceInventoryRow,
	scope EventSourceScope,
	exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != awsSQSSurface || row.HasTraceAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, SQSRedrivePolicyRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickSQSRedrivePolicyPattern(iacpicker.RecommendationContext{
		Provider:       "aws",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(SQSRedrivePolicyRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckCloudTasksRetryPolicy is the detection branch for the GCP Cloud
// Tasks retry-policy kind. Fires on Surface==cloudtasks && !HasTraceAxis
// (no retry policy configured → transient failures aren't retried).
// Terraform from iacpicker.PickCloudTasksRetryPolicyPattern.
func CheckCloudTasksRetryPolicy(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != gcpCloudTasksSurface || row.HasTraceAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, CloudTasksRetryPolicyRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickCloudTasksRetryPolicyPattern(iacpicker.RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(CloudTasksRetryPolicyRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckCloudTasksLogging is the detection branch for the GCP Cloud
// Tasks logging kind. Fires on Surface==cloudtasks && !HasLogAxis (no
// Stackdriver logging sampling → delivery attempts aren't recorded).
// Terraform from iacpicker.PickCloudTasksLoggingPattern.
func CheckCloudTasksLogging(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != gcpCloudTasksSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, CloudTasksLoggingRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickCloudTasksLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(CloudTasksLoggingRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckPubSubLiteLogging is the detection branch for the GCP Pub/Sub
// Lite logging kind. Fires on Surface==pubsublite && !HasLogAxis (no
// Cloud Logging sink filtering on the topic). Terraform from
// iacpicker.PickPubSubLiteLoggingPattern.
func CheckPubSubLiteLogging(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != gcpPubSubLiteSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, PubSubLiteLoggingRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickPubSubLiteLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(PubSubLiteLoggingRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckPubSubLiteReservation is the detection branch for the GCP
// Pub/Sub Lite reservation kind. Fires on Surface==pubsublite &&
// !HasReservation (no throughput reservation → topic throttled to the
// per-partition floor). The signal comes from the scanned Detail bag
// (Detail["has_reservation"]), read into HasReservation by the per-cloud
// row→inventory projection. Terraform from
// iacpicker.PickPubSubLiteReservationPattern (billable — the reasoning
// flags the operator-incurred cost).
func CheckPubSubLiteReservation(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != gcpPubSubLiteSurface || row.HasReservation {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, PubSubLiteReservationRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickPubSubLiteReservationPattern(iacpicker.RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(PubSubLiteReservationRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckEventGridDiagnostics is the detection branch for the Azure Event
// Grid diagnostics kind. Fires on Surface==eventgrid && !HasLogAxis (no
// diagnostic setting routing to Log Analytics / App Insights). Terraform
// from iacpicker.PickEventGridDiagnosticsPattern.
func CheckEventGridDiagnostics(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != azureEventGridSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, EventGridDiagnosticsRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickEventGridDiagnosticsPattern(iacpicker.RecommendationContext{
		Provider:       "azure",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(EventGridDiagnosticsRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckEventGridCloudEventSchema is the detection branch for the Azure
// Event Grid CloudEvents-schema kind. Fires on Surface==eventgrid &&
// !HasTraceAxis — the topic uses a proprietary input_schema
// (EventGridSchema / CustomEventSchema) rather than CloudEventSchemaV1_0,
// so it skips the W3C traceparent extension. Terraform from
// iacpicker.PickEventGridCloudEventSchemaPattern (a BREAKING change for
// subscribers — the reasoning flags coordinate-before-merge).
func CheckEventGridCloudEventSchema(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != azureEventGridSurface || row.HasTraceAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, EventGridCloudEventRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickEventGridCloudEventSchemaPattern(iacpicker.RecommendationContext{
		Provider:       "azure",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(EventGridCloudEventRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckEventHubsDiagnostics is the detection branch for the Azure Event
// Hubs diagnostics kind. Fires on Surface==eventhubs && !HasLogAxis (the
// namespace has no diagnostic setting). Terraform from
// iacpicker.PickEventHubsDiagnosticsPattern.
func CheckEventHubsDiagnostics(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != azureEventHubsSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, EventHubsDiagnosticsRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickEventHubsDiagnosticsPattern(iacpicker.RecommendationContext{
		Provider:       "azure",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(EventHubsDiagnosticsRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckEventHubsCapture is the detection branch for the Azure Event Hubs
// Capture kind. Fires on Surface==eventhubs && !HasCapture — no event
// hub in the namespace has Capture enabled, so raw event bodies aren't
// archived for replay/post-mortem. The signal comes from the scanned
// Detail["has_capture"] bag (not a top-level axis), read into HasCapture
// by the row→inventory projection. Terraform from
// iacpicker.PickEventHubsCapturePattern.
func CheckEventHubsCapture(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != azureEventHubsSurface || row.HasCapture {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, EventHubsCaptureRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickEventHubsCapturePattern(iacpicker.RecommendationContext{
		Provider:       "azure",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(EventHubsCaptureRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckONSLogging is the detection branch for the OCI Notification
// Service logging kind. Fires on Surface==notifications && !HasLogAxis
// (the topic has no OCI Logging configured, so there's no audit trail of
// delivery events). Terraform from iacpicker.PickONSLoggingPattern.
func CheckONSLogging(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != ociNotificationsSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, ONSLoggingRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickONSLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "oci",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(ONSLoggingRecommendationKind, recID, row, scope, terraform, reasoning), nil
}

// CheckQueuesLogging is the detection branch for the OCI Queue Service
// logging kind. Fires on Surface==queues && !HasLogAxis (the queue has
// no OCI Logging configured, so dequeue/DLQ delivery events leave no
// audit trail). Terraform from iacpicker.PickQueuesLoggingPattern.
// Second OCI event-source surface (Streaming + ONS + Queue Service),
// completing the slice-9 OCI event-source parity claim.
func CheckQueuesLogging(
	ctx context.Context, row EventSourceInventoryRow,
	scope EventSourceScope, exclusions EventSourceExclusionStore,
) (*EventSourceRecommendationDraft, error) {
	if row.Surface != ociQueuesSurface || row.HasLogAxis {
		return nil, nil
	}
	recID := resolveRecID(row)
	excluded, err := eventSourceExcluded(ctx, exclusions, scope, recID, QueuesLoggingRecommendationKind)
	if err != nil || excluded {
		return nil, err
	}
	terraform, reasoning := iacpicker.PickQueuesLoggingPattern(iacpicker.RecommendationContext{
		Provider:       "oci",
		ResourceTFName: row.ResourceTFName,
	})
	return buildEventSourceDraft(QueuesLoggingRecommendationKind, recID, row, scope, terraform, reasoning), nil
}
