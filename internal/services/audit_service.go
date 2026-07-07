// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
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

	// VerifyChain walks the caller's tenant audit hash-chain and reports whether
	// it is intact (ADR 0027 slice 1). Self-tenant only; delegates straight to
	// the application store's VerifyAuditChain.
	VerifyChain(ctx context.Context) (*applicationstore.AuditChainVerification, error)
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
	Actor      string // exact-match on actor; backs per-actor access-review timelines (ADR 0020)
	Since      time.Time
	Until      time.Time // Timestamp < Until; symmetric with Since, backs export cursor pagination (ADR 0020)
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

	// v0.89.363 — operator reassigns an agent to a different group (or clears
	// its assignment) via PATCH /api/v1/agents/:id/group. This mutates the
	// stored GroupID/GroupName that rollout canary scoping reads back, so it
	// belongs on the audit timeline alongside agent.registered / drift / delete
	// (every other agent mutation already audits; this one didn't). Emitted by
	// the HTTP handler — NOT the shared AgentService.UpdateAgentRegistration,
	// which the OpAMP heartbeat also calls — and only when the group actually
	// changes (a no-op reassignment to the same group emits nothing). Payload:
	// agent_id, from_group_id, from_group_name, to_group_id, to_group_name
	// (the from_*/to_* pairs are empty strings when clearing/unassigned).
	AuditEventAgentGroupReassigned = "agent.group_reassigned"
	AuditEventConfigStored         = "config.stored"
	AuditEventConfigApplied        = "config.applied"
	AuditEventAlertRuleCreated     = "alert_rule.created"
	AuditEventAlertRuleUpdated     = "alert_rule.updated"
	AuditEventAlertRuleDeleted     = "alert_rule.deleted"
	AuditEventAlertFired           = "alert.fired"
	AuditEventAlertResolved        = "alert.resolved"

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
	AuditEventIaCGitHubConnectionCreated   = "iac.github.connection_created"
	AuditEventIaCGitHubConnectionValidated = "iac.github.connection_validated"
	AuditEventIaCGitHubPlacementMapUpdated = "iac.github.placement_map_updated"
	AuditEventRecommendationPROpened       = "recommendation.pr_opened"
	AuditEventRecommendationPROpenFailed   = "recommendation.pr_open_failed"
	AuditEventRecommendationPRMerged       = "recommendation.pr_merged"

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

	// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — operator-set
	// exclusion for discovery recommendations. Emitted by the
	// POST /api/v1/discovery/aws/recommendations/exclude handler on
	// state transitions only — clicking Don't propose this again on a
	// recommendation that was already excluded is a no-op and emits
	// nothing. The Action verb distinguishes the two transitions
	// ("excluded" vs "exclude_cleared") so SIEM consumers can filter
	// without cracking the payload.
	//
	// Payload contract (SIEM consumers parse on this):
	//   - recommendation_id (string): the deterministic ID the
	//     discovery proposer assigned to the recommendation.
	//   - connection_id (string): the IaC connection scope.
	//   - account_id (string): the AWS account scope.
	//   - region (string): the AWS region scope.
	//   - recommendation_kind (string): the kind the proposer emitted.
	//   - resource_id (string, omitempty): optional resource-level
	//     scope. Empty means kind-level exclusion.
	//   - excluded_by (string): the operator on AuditEventDiscovery-
	//     RecommendationExcluded.
	//   - cleared_by (string): the operator on AuditEventDiscovery-
	//     RecommendationExcludeCleared. Symmetric replacement so the
	//     two events round-trip with the same shape.
	//
	// See docs/proposals/531-proposer-learning-slice2.md §9 (b) and
	// §10 contract item 8.
	AuditEventDiscoveryRecommendationExcluded       = "discovery_recommendation.excluded"
	AuditEventDiscoveryRecommendationExcludeCleared = "discovery_recommendation.exclude_cleared"

	// v0.89.42 (#662 Stream 60, slice 1 chunk 1 of the GitHub Checks
	// API back-signal arc) — three audit event types framing the
	// check-run lifecycle Squadron speaks into GitHub on Squadron-
	// opened PRs. Chunk 1 adds the constants only; chunks 2 / 3 / 4
	// wire the bridge / webhook handler / exclusion handler to
	// actually emit these events. The triplet is intentional:
	//
	//   - .created: emitted on the successful POST that opens the
	//     check run just after the PR open path lands. Payload
	//     (per §8 of the design doc): connection_id, pr_url,
	//     head_sha, check_run_id, recommendation_kind, status
	//     (always "in_progress" at this emit point).
	//   - .updated: emitted on every successful PATCH that moves
	//     the check run forward (merge / close-not-merged / operator-
	//     exclude). Payload mirrors .created plus previous_status /
	//     previous_conclusion / new_status / new_conclusion so SIEM
	//     consumers can reconstruct the transition without cracking
	//     a second row.
	//   - .failed: emitted on any wrapper-returned *CheckRunError.
	//     Payload mirrors .created with check_run_id omitted (no id
	//     when create failed) plus an error_kind discriminator. The
	//     four error_kinds slice 1 emits — "scope_missing",
	//     "rate_limit", "pr_not_found", and "network" — are pinned
	//     by the iac/github wrapper in checks.go so SIEM consumers
	//     can fan out on this field without parsing free-form prose.
	//
	// Fail-open posture: .failed is a sibling of, not a replacement
	// for, the existing recommendation.pr_opened / .pr_merged /
	// .pr_closed_not_merged rows. The PR open / merge / close paths
	// still emit their own audit events regardless of whether the
	// check-run-side call succeeded. See design doc §8 (full payload
	// contracts) and §10 contract item 2.
	AuditEventIaCCheckRunCreated = "iac.check_run.created"
	AuditEventIaCCheckRunUpdated = "iac.check_run.updated"
	AuditEventIaCCheckRunFailed  = "iac.check_run.failed"

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

	// v0.89.46 (#667 Stream 65, GCP discovery slice 1 chunk 1) —
	// audit event types for the first non-AWS discovery arc. Chunk 1
	// adds the constants only; chunks 2 / 3 / 5 (scanner, API
	// handlers, proposer integration) wire the call sites to actually
	// emit these events. The six events mirror the AWS discovery
	// arc's lifecycle one-for-one with the project_id field replacing
	// the AWS account_id field — SIEM consumers that read
	// discovery.aws.* rows can apply the same shape to
	// discovery.gcp.* rows.
	//
	// Payload contract (per design doc §11, §13 acceptance test 8):
	//
	//   - .connection_created: connection_id, display_name, project_id,
	//     region (empty="scan all"). SealedSA bytes NEVER in payload —
	//     the credstore-sealed posture extends to the audit surface.
	//   - .connection_deleted: connection_id, project_id. SealedSA
	//     bytes NEVER in payload (the delete path doesn't touch them
	//     anyway).
	//   - .scan_started: connection_id, project_id, region, scan_id.
	//   - .scan_completed: connection_id, project_id, region, scan_id,
	//     total_resources, instrumented_count, partial (bool),
	//     partial_reason (string, omitempty), failed_services
	//     ([]string of GCP service names like "gce").
	//   - .scan_failed: connection_id, project_id, region, scan_id,
	//     error_kind ("permission_denied" | "project_not_found" |
	//     "network" | "credentials_invalid" | "project_mismatch"),
	//     humanized_message. Plaintext SA JSON NEVER in payload or
	//     error message.
	//   - .recommendations_generated: connection_id, project_id,
	//     region, scan_id, recommendation_count, verdict_examples_used
	//     ([]string of PR URLs — mirrors the AWS proposer's payload
	//     shape so chunk 5's proposer integration slots into the
	//     existing verdict-learning loop without schema changes).
	//
	// See docs/proposals/gcp-discovery-slice1.md §10 contract item 6.
	AuditEventDiscoveryGCPConnectionCreated        = "discovery.gcp.connection_created"
	AuditEventDiscoveryGCPConnectionDeleted        = "discovery.gcp.connection_deleted"
	AuditEventDiscoveryGCPScanStarted              = "discovery.gcp.scan_started"
	AuditEventDiscoveryGCPScanCompleted            = "discovery.gcp.scan_completed"
	AuditEventDiscoveryGCPScanFailed               = "discovery.gcp.scan_failed"
	AuditEventDiscoveryGCPRecommendationsGenerated = "discovery.gcp.recommendations_generated"

	// v0.89.51 (#674 Stream 72, Azure discovery slice 1 chunk 1) —
	// audit event types for the second non-AWS discovery arc, mirroring
	// the GCP arc's lifecycle one-for-one with subscription_id replacing
	// the GCP project_id (and the AWS account_id). Chunk 1 adds the
	// constants only; chunks 2 / 3 / 5 (scanner, API handlers, proposer
	// integration) wire the call sites to actually emit these events.
	// SIEM consumers that already fan out on discovery.aws.* and
	// discovery.gcp.* rows can apply the same shape to discovery.azure.*
	// rows — the only field swap is the cloud-specific scope id.
	//
	// Payload contract (per docs/proposals/azure-discovery-slice1.md
	// §11, §13 contract item 6):
	//
	//   - .connection_created: connection_id, display_name, tenant_id,
	//     subscription_id, client_id, location (empty="scan all").
	//     SealedSecret bytes NEVER in payload — the credstore-sealed
	//     posture extends to the audit surface. The plaintext SP
	//     client_secret is NEVER in payload under any circumstance;
	//     the seal/unseal pair is the only sanctioned access path.
	//   - .connection_deleted: connection_id, tenant_id, subscription_id.
	//     SealedSecret bytes NEVER in payload (the delete path doesn't
	//     touch them anyway).
	//   - .scan_started: connection_id, tenant_id, subscription_id,
	//     location, scan_id.
	//   - .scan_completed: connection_id, tenant_id, subscription_id,
	//     location, scan_id, total_resources, instrumented_count,
	//     partial (bool), partial_reason (string, omitempty),
	//     failed_services ([]string of Azure service names like
	//     "azurevm").
	//   - .scan_failed: connection_id, tenant_id, subscription_id,
	//     location, scan_id, error_kind ("permission_denied" |
	//     "subscription_not_found" | "tenant_invalid" |
	//     "credentials_invalid" | "network"), humanized_message.
	//     Plaintext SP client_secret NEVER in payload or error message.
	//   - .recommendations_generated: connection_id, tenant_id,
	//     subscription_id, location, scan_id, recommendation_count,
	//     verdict_examples_used ([]string of PR URLs — mirrors the
	//     AWS / GCP proposer payload shape so chunk 5's proposer
	//     integration slots into the existing verdict-learning loop
	//     without schema changes).
	//
	// See docs/proposals/azure-discovery-slice1.md §11 (audit events)
	// and §13 contract item 6.
	AuditEventDiscoveryAzureConnectionCreated        = "discovery.azure.connection_created"
	AuditEventDiscoveryAzureConnectionDeleted        = "discovery.azure.connection_deleted"
	AuditEventDiscoveryAzureScanStarted              = "discovery.azure.scan_started"
	AuditEventDiscoveryAzureScanCompleted            = "discovery.azure.scan_completed"
	AuditEventDiscoveryAzureScanFailed               = "discovery.azure.scan_failed"
	AuditEventDiscoveryAzureRecommendationsGenerated = "discovery.azure.recommendations_generated"

	// v0.89.56 (#681 Stream 79, OCI discovery slice 1 chunk 1) —
	// audit event types for the THIRD non-AWS discovery arc, mirroring
	// the GCP and Azure arcs' lifecycles one-for-one with tenancy_ocid
	// replacing the GCP project_id / Azure subscription_id (and the AWS
	// account_id). Chunk 1 adds the constants only; chunks 2 / 3 / 5
	// (scanner, API handlers, proposer integration) wire the call sites
	// to actually emit these events. SIEM consumers that already fan
	// out on discovery.aws.* / discovery.gcp.* / discovery.azure.* rows
	// can apply the same shape to discovery.oci.* rows — the only field
	// swap is the cloud-specific scope id.
	//
	// Payload contract (per docs/proposals/oci-discovery-slice1.md
	// §11, §13 contract item 4):
	//
	//   - .connection_created: connection_id, display_name,
	//     tenancy_ocid, user_ocid, fingerprint, region (REQUIRED for
	//     OCI — regional endpoints mean empty is invalid, not "scan
	//     all"). SealedPrivateKey bytes NEVER in payload — the
	//     credstore-sealed posture extends to the audit surface. The
	//     plaintext RSA private key is NEVER in payload under any
	//     circumstance; the seal/unseal pair is the only sanctioned
	//     access path. Private key bytes are the strongest credential
	//     type Squadron handles.
	//   - .connection_deleted: connection_id, tenancy_ocid, user_ocid.
	//     SealedPrivateKey bytes NEVER in payload (the delete path
	//     doesn't touch them anyway).
	//   - .scan_started: connection_id, tenancy_ocid, user_ocid,
	//     region, scan_id.
	//   - .scan_completed: connection_id, tenancy_ocid, user_ocid,
	//     region, scan_id, total_resources, instrumented_count,
	//     partial (bool), partial_reason (string, omitempty),
	//     failed_services ([]string of OCI service names like
	//     "ocicompute").
	//   - .scan_failed: connection_id, tenancy_ocid, user_ocid,
	//     region, scan_id, error_kind ("permission_denied" |
	//     "tenancy_not_found" | "fingerprint_mismatch" |
	//     "private_key_invalid" | "network"), humanized_message.
	//     Plaintext private key NEVER in payload or error message.
	//   - .recommendations_generated: connection_id, tenancy_ocid,
	//     user_ocid, region, scan_id, recommendation_count,
	//     verdict_examples_used ([]string of PR URLs — mirrors the
	//     AWS / GCP / Azure proposer payload shape so chunk 5's
	//     proposer integration slots into the existing verdict-
	//     learning loop without schema changes).
	//
	// See docs/proposals/oci-discovery-slice1.md §11 (audit events)
	// and §13 contract item 4.
	AuditEventDiscoveryOCIConnectionCreated        = "discovery.oci.connection_created"
	AuditEventDiscoveryOCIConnectionDeleted        = "discovery.oci.connection_deleted"
	AuditEventDiscoveryOCIScanStarted              = "discovery.oci.scan_started"
	AuditEventDiscoveryOCIScanCompleted            = "discovery.oci.scan_completed"
	AuditEventDiscoveryOCIScanFailed               = "discovery.oci.scan_failed"
	AuditEventDiscoveryOCIRecommendationsGenerated = "discovery.oci.recommendations_generated"

	// v0.89.61 (#688 Stream 86, Unified Discovery dashboard slice 1
	// chunk 1) — emitted by the discovery summary handler each time
	// the cache MISSES and a fresh aggregation walk runs. Cache hits
	// are intentionally NOT audited: the aggregation walks four
	// provider stores + an audit-table sweep, and emitting an event
	// per HTTP hit on the /discovery dashboard would drown the
	// timeline. The cache-miss row is the operationally interesting
	// signal — it says "Squadron actually walked all four clouds at
	// this timestamp." Cache-hit calls return the same payload
	// instantly with no audit row.
	//
	// Per docs/proposals/unified-discovery-dashboard-slice1.md §7
	// contract item 4 + §9 acceptance test TestDiscoverySummary_
	// EmitsAuditOnCacheMiss.
	//
	// Actor: "system". TargetType: empty (the summary is fleet-wide,
	// not scoped to one connection). Payload carries provider counts
	// the way the response did so SIEM consumers can reconstruct the
	// aggregate without round-tripping the endpoint.
	AuditEventDiscoverySummaryRequested = "discovery.summary.requested"

	// v0.89.75 (#706 Stream 104, slice 1 chunk 2 of the Trace
	// integration arc) — receiver-wiring + background flush. The
	// trace_index.background_flushed event fires once per flush cycle
	// from the chunk-2 background goroutine
	// (internal/traceindex.BackgroundFlusher). Payload is the meta-
	// shape ONLY — rows_written (int), rows_evicted (int),
	// duration_ms (int), interval_s (int). NO span content, NO
	// resource attributes — design doc §8 + §11 acceptance test 12
	// ("Span content not in audit") pin this.
	//
	// discovery.trace_coverage.requested fires from the chunk-3
	// /api/v1/discovery/trace_coverage handler on cache MISS only,
	// mirroring the cache-miss-only emission pattern AuditEvent-
	// DiscoverySummaryRequested established. Cache hits return the
	// composed payload instantly with no audit row so the timeline
	// doesn't drown in dashboard polls.
	//
	// See docs/proposals/trace-integration-slice1.md §8 (audit
	// events) and §9 contract item 5.
	AuditEventTraceIndexBackgroundFlushed = "trace_index.background_flushed"
	AuditEventTraceCoverageRequested      = "discovery.trace_coverage.requested"

	// v0.89.86 (#717 Stream 115, Span quality slice 1 chunk 2) — the
	// /api/v1/discovery/span_quality dashboard endpoint emits this
	// on cache MISS only. Mirrors AuditEventTraceCoverageRequested's
	// cache-miss-only posture so the timeline doesn't drown in 30s-
	// poll noise; cache hits return the cached payload with no audit
	// row. The per-resource detail endpoint
	// /api/v1/discovery/{provider}/inventory/{kind}/{id}/span_quality
	// does NOT emit — its read pattern is operator-clicked drill-down,
	// not dashboard poll, and the timeline value is lower than the
	// payload-size cost.
	//
	// Payload contract: cache_status ("miss"), total_resource_count,
	// total_resources_with_issues, recorded_at. The percentages are
	// intentionally NOT in the payload — they're high-cardinality
	// floats that bloat the audit timeline without adding signal
	// (the resources_with_issues count answers "is anything wrong").
	AuditEventSpanQualityRequested = "discovery.span_quality.requested"

	// v0.89.132 (#772 Stream 170, Workload Health dashboard panel
	// slice 1 chunk 1) — the /api/v1/discovery/workload_health
	// dashboard endpoint emits this on cache MISS only. Mirrors
	// AuditEventTraceCoverageRequested + AuditEventSpanQualityRequested
	// cache-miss-only posture so the timeline doesn't drown in 30s-
	// poll noise; cache hits return the cached payload with no audit
	// row.
	//
	// Payload contract: cache_status ("miss"),
	// total_serverless_resources, total_any_issue_count,
	// total_any_issue_pct, recorded_at. Same payload-size discipline
	// as AuditEventSpanQualityRequested — the per-diagnostic counts
	// (cold-start / sampling / error-rate) are intentionally NOT in
	// the payload; the any_issue rollup answers "is anything wrong
	// at all" and SIEM consumers can re-fetch the endpoint for the
	// per-diagnostic breakdown.
	AuditEventDiscoveryWorkloadHealthRequested = "discovery.workload_health.requested"

	// v0.89.362 — operator-action audit for the retrospective-savings and
	// cost-spike surfaces. Both mutated state on an operator click (recording
	// an Apply outcome / acknowledging a spike) but emitted no audit row, so
	// the action left no trace on the "what changed when" timeline the rest of
	// Squadron routes through. These close that gap.
	//
	//   - savings.recommendation_applied: emitted by
	//     POST /api/v1/recommendations/:id/applied AFTER the
	//     RecommendationOutcome row is created. Actor is the operator (or
	//     "system"). TargetType AuditTargetRecommendation, TargetID the
	//     recommendation id. Payload: recommendation_id, outcome_id, title,
	//     category, est_savings_per_month_usd_at_apply.
	//   - cost_spike.acknowledged: emitted by
	//     POST /api/v1/alerts/cost-spikes/:id/acknowledge on the FIRST ack
	//     only — an idempotent re-ack of an already-acknowledged spike returns
	//     200 without emitting a duplicate row. TargetType AuditTargetCostSpike,
	//     TargetID the spike id. Payload: cost_spike_id, acknowledged_by.
	AuditEventSavingsRecommendationApplied = "savings.recommendation_applied"
	AuditEventCostSpikeAcknowledged        = "cost_spike.acknowledged"

	// Target types for the two operator-action surfaces above.
	AuditTargetRecommendation = "recommendation"
	AuditTargetCostSpike      = "cost_spike"
)
