// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"
)

// AuditService records and surfaces audit log entries. Every state change in
// Squadron — config push, rule edit, agent registration, drift transition,
// alert firing — should flow through Record() so operators have a single
// "what changed when" timeline to look at.
type AuditService interface {
	// Record persists an audit event with timestamp=now if not set. The
	// payload is freeform and event-type-specific.
	Record(ctx context.Context, entry AuditEntry) error

	// List returns audit events filtered and sorted newest-first.
	List(ctx context.Context, filter AuditEventFilter) ([]*AuditEvent, error)

	// Get returns a single audit event by ID, or nil if no row matches.
	// Added in v0.57 for the audit-explain endpoint which needs to load
	// one row by ID to build the prompt context.
	Get(ctx context.Context, id string) (*AuditEvent, error)

	// SetExplanation persists a cached AI explanation on the row. Audit
	// rows are otherwise immutable; this is the one mutation the service
	// allows, and it only touches the three explanation columns.
	SetExplanation(ctx context.Context, id string, explanation, model string, generatedAt time.Time) error
}

// AuditEntry is the input shape callers fill in to Record. The service
// stamps an ID and timestamp before persisting.
type AuditEntry struct {
	Actor      string         // "system" | "operator:<email>" | "agent:<id>" | "opamp"
	EventType  string         // dotted name, e.g. "config.applied"
	TargetType string         // "agent" | "group" | "config" | "rule"
	TargetID   string         // affected entity id; may be empty for fleet-wide
	Action     string         // "created" | "updated" | "deleted" | "applied" | "drift" | ...
	Payload    map[string]any // optional metadata
}

// AuditEvent is one entry in the log as returned by List.
type AuditEvent struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	Actor      string         `json:"actor"`
	EventType  string         `json:"event_type"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id,omitempty"`
	Action     string         `json:"action"`
	Payload    map[string]any `json:"payload,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`

	// v0.57 — cached AI explanation surface. Empty when the row has not
	// been explained yet; non-empty after the first explain call. The
	// UI shows the cached value directly without round-tripping the LLM
	// again unless the operator clicks Regenerate.
	AIExplanation            string     `json:"ai_explanation,omitempty"`
	AIExplanationModel       string     `json:"ai_explanation_model,omitempty"`
	AIExplanationGeneratedAt *time.Time `json:"ai_explanation_generated_at,omitempty"`
}

// AuditEventFilter narrows a List query. All fields are optional.
type AuditEventFilter struct {
	EventType  string
	TargetType string
	TargetID   string
	Since      time.Time
	Limit      int
}

// Canonical actor values. Use these for events Squadron itself generates so
// the UI can group/filter consistently.
const (
	AuditActorSystem = "system"
	AuditActorOpAMP  = "opamp"
)

// Canonical target type values.
const (
	AuditTargetAgent         = "agent"
	AuditTargetGroup         = "group"
	AuditTargetConfig        = "config"
	AuditTargetRule          = "rule"
	AuditTargetActionRequest = "action_request"
	AuditTargetActionRunner  = "action_runner"
	AuditTargetIncidentDraft = "incident_draft"
)

// Canonical event types. Not exhaustive — callers can use any dotted name
// that makes sense — but having stable constants for the common ones makes
// search and UI filtering reliable.
const (
	AuditEventAgentRegistered   = "agent.registered"
	AuditEventAgentDriftSynced  = "agent.drift.synced"
	AuditEventAgentDriftDrifted = "agent.drift.drifted"
	AuditEventConfigStored      = "config.stored"
	AuditEventConfigApplied     = "config.applied"
	AuditEventAlertRuleCreated  = "alert_rule.created"
	AuditEventAlertRuleUpdated  = "alert_rule.updated"
	AuditEventAlertRuleDeleted  = "alert_rule.deleted"
	AuditEventAlertFired        = "alert.fired"
	AuditEventAlertResolved     = "alert.resolved"

	// Action runner lifecycle. action.dispatched fires when Squadron
	// signs a request and writes it as pending. action.executed and
	// action.failed fire when the runner posts a result; action.denied
	// fires when the runner (or Squadron at dispatch time) refuses to
	// run the request — signature failure, expired request, out of
	// declared capability, or dry-run-only mode.
	AuditEventActionDispatched = "action.dispatched"
	AuditEventActionExecuted   = "action.executed"
	AuditEventActionFailed     = "action.failed"
	AuditEventActionDenied     = "action.denied"

	// Incident drafter lifecycle (Move 3). incident.drafted fires
	// when the bridge persists a new draft; incident.draft_declined
	// fires when the model returned declined=true so the timeline
	// shows Squadron looked at the action even when no ticket was
	// produced.
	AuditEventIncidentDrafted       = "incident.drafted"
	AuditEventIncidentDraftDeclined = "incident.draft_declined"

	// AI proposer lifecycle. proposal.created and proposal.declined
	// fire from the bridge when the LLM actually produced a verdict.
	// proposal.skipped fires when the bridge refused to call the LLM
	// at all because context could not be assembled — the spike's
	// attribution did not resolve to a group, or the group has no
	// current config to propose against. v0.58's stress test surfaced
	// these pre-LLM refusals as a blind spot in the audit timeline;
	// v0.59 makes them visible.
	AuditEventProposalCreated  = "proposal.created"
	AuditEventProposalDeclined = "proposal.declined"
	AuditEventProposalSkipped  = "proposal.skipped"

	// v0.89.3 — Connect IaC repo (Stream 19, #603) audit events.
	// Slice 1 ships four; the webhook-driven pr_merged / pr_closed
	// events the design doc §8 enumerates land with slice 1.5.
	// Payload contracts (per design doc §8) — token bytes are NEVER
	// in any payload, snippet content is NEVER in any payload:
	//   - iac.github.connection_created: connection_id,
	//     repo_full_name, default_branch, auth_kind, placement_map.
	//   - iac.github.connection_validated: repo_full_name,
	//     default_branch, preflight_results[].
	//   - iac.github.placement_map_updated: connection_id,
	//     repo_full_name, placement_map. v0.89.4 (#610) — emitted when
	//     the placement map is edited in-place via the deep-linked
	//     wizard (PATCH /iac/github/connections/:id/placement-map).
	//     Token bytes NEVER in payload; same posture as
	//     iac.github.connection_created.
	//   - recommendation.pr_opened: scan_id, step_idx, account_id,
	//     repo_full_name, pr_number, pr_url, branch, commit_sha,
	//     file_path, actor.
	//   - recommendation.pr_open_failed: as above with error_code +
	//     humanized_message; pr_number omitted when no PR opened.
	AuditEventIaCGitHubConnectionCreated     = "iac.github.connection_created"
	AuditEventIaCGitHubConnectionValidated   = "iac.github.connection_validated"
	AuditEventIaCGitHubPlacementMapUpdated   = "iac.github.placement_map_updated"
	AuditEventRecommendationPROpened         = "recommendation.pr_opened"
	AuditEventRecommendationPROpenFailed     = "recommendation.pr_open_failed"

	// Target type strings for the v0.89.3 IaC events. Used by the
	// timeline humanizer to group connection-lifecycle events
	// (iac.github.connection_*) separately from per-recommendation
	// PR-lifecycle events (recommendation.pr_*).
	AuditTargetIaCConnection     = "iac_connection"
	AuditTargetIaCRecommendation = "iac_recommendation"

	// v0.89.7a — Multi-account AWS scan-all (#616 Stream 21) audit
	// event. The orchestrator's POST /api/v1/discovery/aws/scan-all
	// endpoint emits one of these in addition to the N per-account
	// discovery.aws.scan_completed events the per-account scans
	// produce. Payload carries scan_all_id (the trace link tying
	// the aggregate event to the per-account events),
	// total_accounts, succeeded_accounts (int count),
	// failed_accounts ([]{account_id, error_code, humanized_message}),
	// failed_account_ids (flat []string when non-empty — SIEM
	// forwarders pattern-match on this), total_resources,
	// total_instrumented, total_uninstrumented, partial (bool —
	// true when any failed_accounts), recorded_at. Credential
	// material is NEVER in the payload (the orchestrator never
	// sees cleartext credentials). The per-account scan_completed
	// events still fire unchanged but additionally carry the
	// scan_all_id field via the orchestrator's PerAccountScan
	// callback — operators reading the timeline see N per-account
	// events linked to the aggregate event by the shared
	// scan_all_id.
	AuditEventDiscoveryAWSScanAllCompleted = "discovery.aws.scan_all_completed"

	// AuditTargetDiscoveryScanAll groups the aggregate scan-all
	// events together for timeline filtering. Distinct from
	// credstore.TargetTypeCloudConnection (which the per-account
	// scan_completed events use as their TargetType keyed by the
	// connection's account_id) so the UI can filter
	// "show me only aggregate multi-account events" cleanly. The
	// TargetID of an aggregate event is the scan_all_id; the
	// per-account events' TargetIDs are the individual account_ids
	// — the two views correlate via the scan_all_id payload field.
	AuditTargetDiscoveryScanAll = "discovery_scan_all"
)
