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

	// v0.89.26 (#642 Stream 43) — per-rollout opt-out for the
	// proposer-learns-from-verdicts loop (#531 slice 2 §10 Q3).
	// Emitted by the POST /api/v1/rollouts/:id/exclude-from-learning
	// handler whenever an operator toggles the per-rollout flag
	// (either direction). The Action verb distinguishes the two
	// directions ("exclude_from_learning" or "include_in_learning")
	// so SIEM consumers can fan out without having to crack the
	// payload. Payload contract (SIEM consumers parse on this):
	//   - rollout_id (string): the affected rollout's id.
	//   - previous_state (bool): the rollout's exclude_from_learning
	//     value BEFORE the toggle. Always present so consumers can
	//     reconstruct the transition without an extra Get.
	//   - new_state (bool): the value AFTER the toggle. Equal to
	//     previous_state on a no-op toggle (the handler still emits
	//     so the audit row is unambiguous).
	//   - reason (string, omitempty): the operator's optional human-
	//     readable note. Omitted from the payload when empty so the
	//     v1 UI's "no reason field" call lands a clean row. Scripted
	//     callers can pass a reason for forensics.
	AuditEventRolloutExcludedFromLearning = "rollout.excluded_from_learning"

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
	//     file_path, actor. v0.89.11 (#626 Stream 27) adds
	//     disposition ("new_file" | "patch_existing"),
	//     manual_merge_required (bool — true for patch_existing),
	//     and on new_file also created_file_path naming the new
	//     sibling file Squadron wrote.
	//   - recommendation.pr_open_failed: as above with error_code +
	//     humanized_message; pr_number omitted when no PR opened.
	//     v0.89.11 adds disposition keyed off ResourceKind for
	//     auditor-side correlation.
	//   - recommendation.pr_merged: v0.89.23 (#639 Stream 40) — emitted
	//     by the GitHub webhook listener when an operator merges a
	//     PR Squadron either authored or (more generally) any PR in
	//     the connected repo whose branch matches the configured
	//     Squadron prefix. Payload: repo_full_name, pr_number,
	//     pr_url, branch, merged_at (RFC3339 string from GitHub),
	//     merged_by (GitHub login string), recommendation_kind
	//     (parsed from the branch's first path segment after the
	//     Squadron prefix; empty when the branch isn't Squadron-
	//     shaped), connection_id (the matched iac_connection's id;
	//     empty when no connection matches the repo). Token bytes
	//     and webhook-secret bytes NEVER in payload; the HMAC is
	//     validated and discarded before this row is written.
	AuditEventIaCGitHubConnectionCreated     = "iac.github.connection_created"
	AuditEventIaCGitHubConnectionValidated   = "iac.github.connection_validated"
	AuditEventIaCGitHubPlacementMapUpdated   = "iac.github.placement_map_updated"
	AuditEventRecommendationPROpened         = "recommendation.pr_opened"
	AuditEventRecommendationPROpenFailed     = "recommendation.pr_open_failed"
	AuditEventRecommendationPRMerged         = "recommendation.pr_merged"

	// AuditEventRecommendationPRClosedNotMerged — v0.89.36 (#655
	// Stream 53, #531 slice 2 chunk 3) — recorded by the
	// IaCGitHubWebhookHandler when a pull_request.closed delivery
	// arrives with pull_request.merged=false. Payload mirrors
	// recommendation.pr_merged exactly with `closed_at` and
	// `closed_by` replacing `merged_at` and `merged_by`. The
	// discovery proposer's ListDiscoveryVerdicts query unions this
	// event type with recommendation.pr_merged and projects rows
	// into the rejected bucket via verdictsel.StateClosedNotMerged,
	// surfacing the prompt block's `[CLOSED_NOT_MERGED]` stanza per
	// docs/proposals/531-proposer-learning-slice2.md §7.2.
	AuditEventRecommendationPRClosedNotMerged = "recommendation.pr_closed_not_merged"

	// AuditEventWebhookDeliveryReplayed — v0.89.30 (#649) — records
	// inbound webhook deliveries that passed HMAC verification but
	// whose X-GitHub-Delivery UUID was already present in the dedupe
	// table. Closes the replay-attack threat the slice-1 webhook
	// receiver explicitly left on the table (compromised TLS
	// terminator or intermediary proxy captures + replays a
	// legitimate signed delivery).
	//
	// Emitted by the IaCGitHubWebhookHandler AFTER signature
	// verification + AFTER the dedupe insert returns firstTime=false.
	// The receiver then returns 200 with body
	//   {"ok": true, "ignored": true, "reason": "replayed",
	//    "delivery_id": <id>}
	// and does NOT proceed to the event-type filter or audit-emit
	// path. 200 (not 4xx/5xx) so GitHub's redelivery system reads
	// the response as "delivered" and doesn't keep retrying.
	//
	// Payload fields:
	//   - delivery_id (string): the X-GitHub-Delivery UUID that
	//     collided.
	//   - event_type (string): the X-GitHub-Event header value the
	//     replay attempt carried.
	//   - original_received_at (RFC3339 string): the timestamp the
	//     legitimate delivery was originally recorded at, fetched
	//     from the dedupe row. Lets SIEM consumers see how long
	//     elapsed between the original delivery and the replay.
	//
	// Actor: "github_webhook". TargetType:
	// AuditTargetIaCRecommendation (groups with the pr_opened /
	// pr_merged family on the timeline humanizer). TargetID empty —
	// replays aren't tied to a specific connection because the
	// receiver short-circuits before the connection lookup.
	AuditEventWebhookDeliveryReplayed = "webhook.delivery_replayed"

	// v0.89.28 (#643 slice 1) — Discovery proposer learns from accepted
	// recommendations. Emitted by the discovery recommendations handler
	// (POST /api/v1/discovery/aws/connections/:id/recommendations) AFTER
	// the proposer returns, regardless of whether the model accepted or
	// declined. Mirrors the cost-spike side's proposal.created shape so
	// SIEM consumers see the same fields across both proposer surfaces,
	// but the ID scheme on verdict_examples_used DIFFERS — discovery
	// carries PR URLs (the identifying handle for an accepted discovery
	// recommendation; see §11 Q5 of the spec) whereas the cost-spike
	// side's proposal.created carries opaque rollout IDs. Don't conflate
	// the two — different event types, different ID schemes.
	//
	// Payload contract:
	//   - scan_id (string): the discovery scan that produced the
	//     recommendations.
	//   - connection_id (string): the IaC connection_id used as the
	//     scope for the accepted-recommendations lookup.
	//   - account_id (string): the AWS account scanned.
	//   - region (string): the AWS region scanned (slice 1 ships
	//     single-region scans; multi-region scans land in a later slice).
	//   - recommendation_count (int): the number of recommendation rows
	//     the proposer returned (0 when declined).
	//   - verdict_examples_used ([]string of PR URLs): the prior accepted
	//     PRs whose merges informed the proposer's prompt block. ALWAYS
	//     present (never omitted) — empty array on cold start, opt-out,
	//     or recency-window empty so SIEM consumers can filter on the
	//     empty slice to find cold-start cases.
	AuditEventDiscoveryProposalCreated = "discovery_proposal.created"

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
