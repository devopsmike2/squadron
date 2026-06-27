package types

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/google/uuid"
)

// ApplicationStore interface for managing application data
type ApplicationStore interface {
	CreateAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, id uuid.UUID) (*Agent, error)
	ListAgents(ctx context.Context) ([]*Agent, error)
	UpdateAgentStatus(ctx context.Context, id uuid.UUID, status AgentStatus) error
	UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error
	UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error
	DeleteAgent(ctx context.Context, id uuid.UUID) error

	// Group management
	CreateGroup(ctx context.Context, group *Group) error
	GetGroup(ctx context.Context, id string) (*Group, error)
	ListGroups(ctx context.Context) ([]*Group, error)
	// UpdateGroup mutates name, labels, require_approval on an
	// existing group. Added in v0.48 for the approval-policy
	// toggle on Groups settings.
	UpdateGroup(ctx context.Context, group *Group) error
	DeleteGroup(ctx context.Context, id string) error

	// Config management
	CreateConfig(ctx context.Context, config *Config) error
	GetConfig(ctx context.Context, id string) (*Config, error)
	GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*Config, error)
	GetLatestConfigForGroup(ctx context.Context, groupID string) (*Config, error)
	ListConfigs(ctx context.Context, filter ConfigFilter) ([]*Config, error)
	ListSavedQueries(ctx context.Context) ([]*SavedQuery, error)
	GetSavedQuery(ctx context.Context, id string) (*SavedQuery, error)
	CreateSavedQuery(ctx context.Context, query *SavedQuery) error
	UpdateSavedQuery(ctx context.Context, query *SavedQuery) error
	DeleteSavedQuery(ctx context.Context, id string) error

	// Alert rule management
	CreateAlertRule(ctx context.Context, rule *AlertRule) error
	GetAlertRule(ctx context.Context, id string) (*AlertRule, error)
	ListAlertRules(ctx context.Context) ([]*AlertRule, error)
	UpdateAlertRule(ctx context.Context, rule *AlertRule) error
	DeleteAlertRule(ctx context.Context, id string) error

	// Audit log
	CreateAuditEvent(ctx context.Context, event *AuditEvent) error
	ListAuditEvents(ctx context.Context, filter AuditEventFilter) ([]*AuditEvent, error)
	GetAuditEvent(ctx context.Context, id string) (*AuditEvent, error)
	UpdateAuditEventExplanation(ctx context.Context, id, explanation, model string, generatedAt time.Time) error

	// Rollouts (safe staged config rollouts)
	CreateRollout(ctx context.Context, rollout *Rollout) error
	GetRollout(ctx context.Context, id string) (*Rollout, error)
	ListRollouts(ctx context.Context, filter RolloutFilter) ([]*Rollout, error)
	UpdateRollout(ctx context.Context, rollout *Rollout) error

	// v0.89.17 (#633) — proposer learns from accepted/rejected verdicts.
	// ListAIVerdictsForGroup returns AI-originated rollouts on the
	// supplied group that have a terminal approval verdict (approved
	// or rejected) recorded after the `since` cutoff, newest verdict
	// first. The proposer bridge uses this to assemble the prior-
	// verdicts few-shot block on the next cost-spike proposal. See
	// docs/proposals/531-proposer-learns-from-accepted-rejected.md §4.
	ListAIVerdictsForGroup(ctx context.Context, groupID string, since time.Time, limit int) ([]*Rollout, error)

	// v0.89.28 (#643 slice 1) — discovery proposer learns from accepted
	// recommendations. v0.89.36 (#655 Stream 53, #531 slice 2 chunk 3)
	// renames + widens this to ListDiscoveryVerdicts: the same scope-
	// filtered sweep now UNIONS both recommendation.pr_merged AND the
	// new recommendation.pr_closed_not_merged audit_events rows,
	// returning the shared DiscoveryVerdict projection with State set
	// per row. The discovery proposer's
	// Bridge.assembleDiscoveryVerdicts calls this to stitch the §6/§7
	// prompt block through verdictsel.Select + verdictprompt.Render.
	//
	// Empty result on cold start (zero matching rows) is honest —
	// the caller produces a byte-for-byte-unchanged prompt in that
	// case. See docs/proposals/531-proposer-learning-slice2.md §5.2.
	ListDiscoveryVerdicts(
		ctx context.Context,
		connectionID, accountID, region string,
		since time.Time, limit int,
	) ([]*DiscoveryVerdict, error)

	// ListCrossScopeDiscoveryVerdicts — cross-cloud citations (v0.89.248).
	// Recent verdicts from connections OTHER than excludeConnectionID, each
	// tagged with origin Provider + ScopeID, so a decline on one cloud can
	// surface (origin-labeled) in another cloud's verdict block.
	ListCrossScopeDiscoveryVerdicts(
		ctx context.Context,
		excludeConnectionID string,
		since time.Time, limit int,
	) ([]*DiscoveryVerdict, error)

	// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — operator-set
	// exclusion infrastructure for discovery recommendations. The
	// "Don't propose this again" affordance on the Recommendations tab
	// POSTs through the new exclude handler; the handler routes through
	// these two methods to upsert / list rows in the new
	// iac_recommendation_verdicts table.
	//
	// SetRecommendationExclusion is the upsert: the projection carries
	// the full row shape (recommendation_id is the PK; scope tuple +
	// kind + resource_id are required; excluded_at + excluded_by carry
	// the "as of" stamp). The bool excluded parameter is the desired
	// final state — true on click, false on un-click. The store stamps
	// excluded_at + excluded_by from the projection only on a transition
	// to true; on a transition to false those two fields are cleared.
	// updated_at is always refreshed.
	//
	// The return value prevExcluded carries the row's exclude_from_learning
	// value BEFORE the upsert (false on insert; the prior column value
	// on update). The handler uses this to decide whether to emit the
	// discovery_recommendation.excluded / .exclude_cleared audit event
	// (transitions only) or skip the audit emit (no-op toggles).
	//
	// See docs/proposals/531-proposer-learning-slice2.md §10 contract
	// items 7 + 8.
	SetRecommendationExclusion(ctx context.Context, rec ExcludedRecommendation, excluded bool) (prevExcluded bool, err error)

	// ListExcludedRecommendations returns the rows in the supplied
	// (connection_id, account_id, region) scope with
	// exclude_from_learning=1, ordered by excluded_at DESC, capped at
	// limit. Empty scope tuple returns no rows (the discovery bridge's
	// short-circuit path); limit<=0 falls through to a small default.
	//
	// The bridge calls this on every discovery proposal call; the
	// idx_iac_rec_verdicts_scope partial-ish index keeps the sweep
	// cheap even on a deployment that has accumulated many exclusions.
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]ExcludedRecommendation, error)

	// v0.89.42 (#662 Stream 60, slice 1 chunk 1 of the GitHub Checks
	// API back-signal arc) — durable check-run state for one
	// recommendation. The lifecycle is intentionally tied to the same
	// iac_recommendation_verdicts row that carries the operator-set
	// exclusion flag (chunk 4): every recommendation Squadron speaks
	// on (opened a PR for, learned a verdict on, or had the operator
	// exclude) gets exactly one row, joined on recommendation_id.
	//
	// SetCheckRunForRecommendation upserts the check-run state for a
	// recommendation. The recommendation_id row is created if it does
	// not exist yet (with exclude_from_learning=0 and excluded_at /
	// excluded_by both NULL — see §11 Q3 of the design doc for the
	// "option A" decision: durable storage on PR open rather than a
	// transient in-memory map). status and conclusion can be empty
	// strings during transient states (the GitHub Checks API treats
	// status="in_progress" with conclusion="" as valid). The rec
	// projection carries the full scope tuple so the row inserts
	// cleanly; on the upsert path the scope fields are NOT overwritten
	// because the underlying row's scope is invariant once persisted.
	//
	// See docs/proposals/checks-api-back-signal.md §6, §10 contract
	// item 4, and §11 open question 3.
	SetCheckRunForRecommendation(ctx context.Context,
		rec ExcludedRecommendation,
		ref CheckRunRef,
		status, conclusion string,
	) error

	// GetCheckRunForRecommendation returns the stored check-run ref +
	// current status / conclusion for recommendationID. exists=false
	// (with no error) when no row matches — this is the cold-start /
	// "PR open path never reached chunk 2" signal the webhook handler
	// and exclusion handler (chunks 2-4) read to decide whether to
	// patch a check run on the inbound merge / close / exclude event.
	//
	// The CheckRunRef returned has its zero value when the row exists
	// but no check_run_id has been written yet (e.g., the row was
	// created by the chunk-4 exclusion path, before the chunk-2
	// bridge wired the create-on-open). exists distinguishes "row not
	// present" from "row present, no check run yet."
	GetCheckRunForRecommendation(ctx context.Context,
		recommendationID string,
	) (ref CheckRunRef, status string, conclusion string, exists bool, err error)

	// v0.89.74 (#705 Stream 103, slice 1 chunk 1 of the Trace
	// integration arc) — trace_resource_seen storage layer. The new
	// internal/traceindex package's Index flushes its in-memory write-
	// through cache through UpsertTraceResources every 30s; the
	// Discovery dashboard's TRACE COVERAGE panel and the per-provider
	// Inventory tabs' last_seen_at column read through the other three.
	//
	// UpsertTraceResources accepts a batch of ResourceRow projections
	// and applies INSERT ... ON CONFLICT(resource_key) DO UPDATE with
	// span_count_24h += new.span_count_24h, last_seen_at + attributes_json
	// + updated_at refreshed to the new values, and first_seen_at
	// preserved. After the upsert, if the row count exceeds the storage
	// layer's max-rows cap (SQUADRON_TRACEINDEX_MAX_ROWS, default 100K
	// per design doc §12) the layer DELETEs the oldest last_seen_at
	// rows until the count drops to the cap. evicted returns the count
	// removed by that sweep — zero on the common path; non-zero
	// triggers the chunk-2 flush audit payload's eviction_count field.
	//
	// GetTraceResource returns the row for resource_key or nil when no
	// row matches. ListTraceResourcesByScope returns rows for a
	// (provider, scope_id) tuple with last_seen_at >= since, ordered
	// newest-first, capped at limit. CountTraceResourcesByScope returns
	// the row count for the same tuple — the dashboard's coverage_pct
	// numerator.
	//
	// See docs/proposals/trace-integration-slice1.md §4, §9 contract
	// items 1-3, and §11 acceptance tests 1-4 + 6.
	UpsertTraceResources(ctx context.Context, rows []traceindex.ResourceRow) (evicted int, err error)
	GetTraceResource(ctx context.Context, key string) (*traceindex.ResourceRow, error)
	ListTraceResourcesByScope(ctx context.Context, provider, scopeID string, since time.Time, limit int) ([]traceindex.ResourceRow, error)
	CountTraceResourcesByScope(ctx context.Context, provider, scopeID string) (int, error)

	// Action runners + requests (v0.53 Move 2). An action runner is
	// an installed squadron-action-runner daemon registered with
	// this Squadron instance. An action request is one signed action
	// dispatch plus its eventual result. List filtering supports
	// the UI's "show all requests for proposal X" and "show in-
	// flight requests assigned to runner Y" patterns.
	CreateActionRunnerRegistration(ctx context.Context, r *ActionRunnerRegistration) error
	UpdateActionRunnerRegistration(ctx context.Context, r *ActionRunnerRegistration) error
	GetActionRunnerRegistration(ctx context.Context, runnerID string) (*ActionRunnerRegistration, error)
	ListActionRunnerRegistrations(ctx context.Context) ([]*ActionRunnerRegistration, error)
	RevokeActionRunnerRegistration(ctx context.Context, runnerID string, at time.Time) error

	CreateActionRequest(ctx context.Context, r *ActionRequest) error
	UpdateActionRequest(ctx context.Context, r *ActionRequest) error
	GetActionRequest(ctx context.Context, id string) (*ActionRequest, error)
	ListActionRequests(ctx context.Context, filter ActionRequestFilter) ([]*ActionRequest, error)

	// SQ-3 incident drafts. One draft per action by default; the
	// bridge dedups on action_request_id so flapping cannot flood
	// the inbox. See docs/incident-drafter-design.md.
	CreateIncidentDraft(ctx context.Context, d *IncidentDraft) error
	UpdateIncidentDraft(ctx context.Context, d *IncidentDraft) error
	GetIncidentDraft(ctx context.Context, id string) (*IncidentDraft, error)
	GetIncidentDraftByActionRequestID(ctx context.Context, actionRequestID string) (*IncidentDraft, error)
	ListIncidentDrafts(ctx context.Context, filter IncidentDraftFilter) ([]*IncidentDraft, error)

	// API tokens (bearer auth)
	CreateAPIToken(ctx context.Context, token *APIToken) error
	GetAPITokenByHash(ctx context.Context, hash string) (*APIToken, error)
	ListAPITokens(ctx context.Context) ([]*APIToken, error)
	UpdateAPITokenLastUsed(ctx context.Context, id string, at time.Time) error
	RevokeAPIToken(ctx context.Context, id string, at time.Time) error

	// Recommendation dismissals (v0.25 cost-recommendations engine).
	// The dismissals table stores one row per (recommendation_id,
	// dismissed_by) so an operator can hide a recommendation that
	// keeps surfacing — and another operator (or the same one after
	// a config change) can restore it explicitly. IDs are the
	// engine's deterministic hash, not random UUIDs, so dismissals
	// stay correlated across re-evaluations.
	DismissRecommendation(ctx context.Context, d *RecommendationDismissal) error
	RestoreRecommendation(ctx context.Context, recommendationID string) error
	IsRecommendationDismissed(ctx context.Context, recommendationID string) (bool, error)
	ListRecommendationDismissals(ctx context.Context) ([]*RecommendationDismissal, error)

	// Recommendation outcomes (v0.28 retrospective savings tracker).
	// Each row records ONE Apply click on a recommendation, with
	// a frozen snapshot of the engine's view at that moment and a
	// running update of the actual observed byte rate. Realized
	// savings is computed from (baseline - observed) × pricing.
	CreateRecommendationOutcome(ctx context.Context, o *RecommendationOutcome) error
	UpdateRecommendationOutcome(ctx context.Context, o *RecommendationOutcome) error
	ListRecommendationOutcomes(ctx context.Context) ([]*RecommendationOutcome, error)

	// Cost-spike events (v0.29 cost-spike alerting). One row per
	// detected anomaly. Open spikes have EndedAt == nil; the
	// detector closes a spike when the current projection drops
	// back below the warn threshold. AcknowledgedAt is operator
	// action — the spike continues to track until it auto-closes.
	CreateCostSpikeEvent(ctx context.Context, e *CostSpikeEvent) error
	UpdateCostSpikeEvent(ctx context.Context, e *CostSpikeEvent) error
	GetCostSpikeEvent(ctx context.Context, id string) (*CostSpikeEvent, error)
	ListCostSpikeEvents(ctx context.Context, filter CostSpikeFilter) ([]*CostSpikeEvent, error)
	// LatestOpenCostSpike returns the most recent open spike (no
	// ended_at), or nil if none exists. Used by the detector to
	// decide whether to append a peak update or open a fresh event.
	LatestOpenCostSpike(ctx context.Context) (*CostSpikeEvent, error)

	// Expected agents (v0.32 inventory reconciliation). The CI/CD
	// pipeline that deploys OTel collectors POSTs its target host
	// list to /api/v1/inventory/expected — we store it here. The
	// reconciliation service then diffs the expected list against
	// the actual agent table to flag missing hosts (in the expected
	// list, never connected or quiet) and unexpected hosts (showing
	// up at OpAMP but not in the expected list).
	//
	// Source identifies which pipeline submitted the entry — a
	// single Squadron can serve multiple deployment pipelines, each
	// owning a slice of the inventory.
	UpsertExpectedAgent(ctx context.Context, e *ExpectedAgent) error
	DeleteExpectedAgent(ctx context.Context, hostname string) error
	ListExpectedAgents(ctx context.Context, source string) ([]*ExpectedAgent, error)
	// ReplaceExpectedAgentsForSource is the atomic "rotate" used by
	// CI: delete every entry with this source, then bulk-insert the
	// new list. Idempotent on the wire.
	ReplaceExpectedAgentsForSource(ctx context.Context, source string, entries []*ExpectedAgent) error

	// Deploy targets + runs (v0.34 GitHub Actions integration).
	// A target encapsulates "the workflow Squadron is allowed to
	// dispatch on your behalf" — the GitHub coordinates plus the
	// encrypted credential. A run records one dispatch with its
	// lifecycle, the resolved inputs, and the expected-hostname set
	// so v0.32 inventory reconciliation can close the loop after the
	// deploy completes.
	CreateDeployTarget(ctx context.Context, t *DeployTarget) error
	UpdateDeployTarget(ctx context.Context, t *DeployTarget) error
	GetDeployTarget(ctx context.Context, id string) (*DeployTarget, error)
	ListDeployTargets(ctx context.Context) ([]*DeployTarget, error)
	DeleteDeployTarget(ctx context.Context, id string) error

	CreateDeployRun(ctx context.Context, r *DeployRun) error
	UpdateDeployRun(ctx context.Context, r *DeployRun) error
	GetDeployRun(ctx context.Context, id string) (*DeployRun, error)
	ListDeployRuns(ctx context.Context, filter DeployRunFilter) ([]*DeployRun, error)

	// v0.89.30 (#649) — webhook replay protection.
	//
	// RecordWebhookDelivery records an inbound webhook delivery by its
	// X-GitHub-Delivery UUID. Returns firstTime=true and receivedAt =
	// the freshly-stamped CURRENT_TIMESTAMP when the row was new
	// (legitimate delivery); firstTime=false and receivedAt = the
	// timestamp the original delivery was recorded at when the
	// delivery_id was already present (a replay). The receiver feeds
	// the prior receivedAt into the AuditEventWebhookDeliveryReplayed
	// payload's original_received_at field so SIEM consumers can see
	// how long the gap was between the legitimate fire and the replay.
	//
	// Implementations MUST atomically insert-and-fetch (or check-and-
	// insert under a lock) so a concurrent race between two replays of
	// the same delivery_id can't both observe firstTime=true. The
	// SQLite implementation relies on INSERT OR IGNORE + RowsAffected;
	// the memory implementation holds the store-wide mutex across the
	// check + insert.
	RecordWebhookDelivery(ctx context.Context, deliveryID, eventType string) (firstTime bool, receivedAt time.Time, err error)

	// GCWebhookDeliveries deletes dedupe rows older than the supplied
	// cutoff. Returns the count of rows deleted. Called from a
	// background loop (24h cadence; 7-day retention) so the dedupe
	// table doesn't grow unbounded across the deployment lifetime.
	// Re-running with a stale cutoff is a clean no-op (deleted=0).
	// v0.89.30 (#649).
	GCWebhookDeliveries(ctx context.Context, before time.Time) (deleted int, err error)

	// SIEM destinations (v0.50 audit export). One row per configured
	// downstream SIEM (Splunk HEC, signed webhook). Secret is the
	// encrypted-at-rest credential — never returned to the API layer
	// in plaintext.
	CreateSiemDestination(ctx context.Context, d *SiemDestination) error
	GetSiemDestination(ctx context.Context, id string) (*SiemDestination, error)
	ListSiemDestinations(ctx context.Context) ([]*SiemDestination, error)
	UpdateSiemDestination(ctx context.Context, d *SiemDestination) error
	DeleteSiemDestination(ctx context.Context, id string) error
	// UpdateSiemDestinationStatus is a narrow update path the
	// dispatcher uses to write LastEventSentAt / LastError / LastErrorAt
	// without touching the rest of the row (avoids racing with an
	// operator's concurrent edit).
	UpdateSiemDestinationStatus(ctx context.Context, id string, sentAt *time.Time, errMsg string, errAt *time.Time) error
}

// DeployTarget describes one GitHub Actions workflow Squadron is
// authorized to dispatch. The encrypted PAT lives in
// EncryptedCredential; nothing outside the deploy package decrypts
// it. Default inputs are merged with the trigger request at runtime
// so common boilerplate (region, env, etc.) can be set once.
//
// Provider is currently always "github" — the column exists so a
// future Jenkins/GitLab provider can sit alongside without a
// migration.
type DeployTarget struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Provider            string            `json:"provider"`
	GitHubOwner         string            `json:"github_owner"`
	GitHubRepo          string            `json:"github_repo"`
	GitHubWorkflow      string            `json:"github_workflow"` // workflow file name e.g. "deploy-otel.yml"
	GitHubBranch        string            `json:"github_branch"`   // ref to dispatch on; default "main"
	EncryptedCredential []byte            `json:"-"`               // nonce(24) || ciphertext; not surfaced via JSON
	HasCredential       bool              `json:"has_credential"`  // computed at read time so the UI can render a "Replace token" affordance without seeing the bytes
	DefaultInputs       map[string]string `json:"default_inputs,omitempty"`
	ConfigID            string            `json:"config_id,omitempty"` // optional pinned Squadron config that gets lint-checked
	// InventoryPath points at an Ansible inventory file inside the
	// target repo (e.g. "winOtel/ansible/inventory.ini"). When set,
	// Squadron fetches that file from GitHub at trigger time, parses
	// the host list out, and auto-populates ExpectedHosts on the
	// resulting deploy run. The trigger UI shows the parsed hosts
	// read-only so the operator can verify the deploy scope before
	// firing. Matches the workflow pattern where inventory.ini is a
	// checked-in file rather than a workflow_dispatch input.
	InventoryPath string    `json:"inventory_path,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DeployRun is one workflow_dispatch firing. Status tracks the
// GitHub Actions lifecycle (queued → in_progress → completed) and
// Conclusion the terminal outcome (success / failure / cancelled /
// timed_out / skipped).
//
// ExpectedHosts is the set Squadron auto-registers into the v0.32
// expected_agents table on success — closing the loop so the
// inventory reconciliation surface flags any host that the workflow
// claimed to deploy but never checks in via OpAMP.
type DeployRun struct {
	ID                string            `json:"id"`
	TargetID          string            `json:"target_id"`
	TargetName        string            `json:"target_name,omitempty"` // resolved on read for the UI
	RequestedBy       string            `json:"requested_by"`
	RequestedAt       time.Time         `json:"requested_at"`
	Inputs            map[string]string `json:"inputs,omitempty"`
	GitHubRunID       int64             `json:"github_run_id,omitempty"`
	GitHubRunURL      string            `json:"github_run_url,omitempty"`
	Status            string            `json:"status"`               // "queued" | "in_progress" | "completed"
	Conclusion        string            `json:"conclusion,omitempty"` // "success" | "failure" | "cancelled" | "timed_out" | "skipped"
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	ExpectedHosts     []string          `json:"expected_hosts,omitempty"`
	VerificationState string            `json:"verification_state,omitempty"` // "pending" | "verified" | "missing_agents"
	VerifiedAt        *time.Time        `json:"verified_at,omitempty"`
	Notes             string            `json:"notes,omitempty"`
}

// DeployRunFilter narrows ListDeployRuns. All fields are optional;
// the zero filter lists every run newest-first.
type DeployRunFilter struct {
	TargetID string
	Status   string
	Limit    int
}

// ExpectedAgent is one row of the v0.32 inventory table — a host
// some CI/CD pipeline promised would be running a collector. The
// reconciliation service diffs this against the actual agent table
// to produce the "missing hosts" and "unexpected hosts" views.
//
// Hostname is the natural key. We don't try to correlate by IP or
// UUID because the CI pipeline only knows the hostname it deployed
// to; the OpAMP-discovered agent UUID isn't known until the
// collector dials in.
type ExpectedAgent struct {
	Hostname      string            `json:"hostname"`
	Labels        map[string]string `json:"labels,omitempty"`
	Source        string            `json:"source"` // which pipeline pushed this row
	ExpectedSince time.Time         `json:"expected_since"`
	UpdatedAt     time.Time         `json:"updated_at"`
	// Notes is a free-form human-readable hint the pipeline can
	// pass through ("staging", "canary", "from job#1234"). Surfaced
	// on the reconciliation panel so an operator triaging a missing
	// host has the context they need.
	Notes string `json:"notes,omitempty"`
}

// RecommendationOutcome is the post-apply tracking record for the
// v0.28 retrospective savings dashboard. Each Apply click creates
// one row; a background poller (or on-demand on /savings/realized
// hits) updates LastObservedBytesPerHour + RealizedSavingsPerMonthUSD
// against the latest insights snapshot.
//
// Status lifecycle:
//
//	pending      → just created, no observation yet
//	realized     → observed byte rate dropped below baseline; savings active
//	not_observed → 24h+ since apply, byte rate hasn't dropped (operator
//	               may have decided not to roll out, or the rollout
//	               hasn't reached the affected agents yet)
//	reverted     → observed byte rate is back near baseline AFTER once
//	               being realized (rollback or config drift)
type RecommendationOutcome struct {
	ID               string    `json:"id"`
	RecommendationID string    `json:"recommendation_id"` // engine's deterministic hash
	AppliedAt        time.Time `json:"applied_at"`
	AppliedBy        string    `json:"applied_by"` // actor string

	// Frozen snapshot at apply time. We freeze these because the
	// engine may stop producing this exact recommendation after the
	// fix lands (which is the whole point) — we still need to
	// describe the outcome in the UI.
	Title                        string  `json:"title"`
	Category                     string  `json:"category"`
	Signal                       string  `json:"signal,omitempty"`        // 'metrics' | 'logs' | 'traces' | ''
	AttributeKey                 string  `json:"attribute_key,omitempty"` // for noisy_attribute recs
	BaselineBytesPerHour         int64   `json:"baseline_bytes_per_hour"`
	EstSavingsPerMonthUSDAtApply float64 `json:"est_savings_per_month_usd_at_apply"`

	// Running observations. Updated periodically.
	LastObservedBytesPerHour   int64     `json:"last_observed_bytes_per_hour"`
	LastObservedAt             time.Time `json:"last_observed_at,omitempty"`
	RealizedSavingsPerMonthUSD float64   `json:"realized_savings_per_month_usd"`
	Status                     string    `json:"status"` // pending|realized|not_observed|reverted
}

// RecommendationDismissal is one operator-driven hide. The engine
// keeps regenerating recommendations on every Evaluate; dismissals
// are how operators say "I know about this, stop showing it".
// Restoring (UI: "show again" / API: RestoreRecommendation) deletes
// the row — we don't keep history of restores because the audit
// log already records both events as RECOMMENDATION_DISMISSED /
// RECOMMENDATION_RESTORED.
type RecommendationDismissal struct {
	// RecommendationID is the engine's deterministic ID. Same input
	// fleet shape → same ID across runs, so this lookup is stable.
	RecommendationID string    `json:"recommendation_id"`
	DismissedAt      time.Time `json:"dismissed_at"`
	DismissedBy      string    `json:"dismissed_by"`     // actor identifier ("operator:email", "system", etc.)
	Reason           string    `json:"reason,omitempty"` // optional free-text reason
}

// APIToken is one issued bearer token. Plaintext token values are NEVER
// stored — only the sha256 hex digest. The plaintext is shown to the
// operator once at creation time.
//
// Lifecycle: created with RevokedAt nil. Revocation sets RevokedAt to
// the moment of revocation; the row is kept so audit history can
// resolve token IDs to labels long after revocation. LastUsedAt is
// best-effort: the middleware updates it on each successful
// authentication, but we don't fail requests if the update errors.
//
// Scopes is the list of permission scopes the token carries (e.g.
// "agents:read", "rollouts:write"). Empty/nil scopes is treated as
// FULL ACCESS for backward compatibility with tokens issued before
// v0.10. New tokens issued via the UI / CLI / API always specify
// scopes explicitly; the empty-equals-full case exists only so
// existing pre-v0.10 deployments don't have every token suddenly
// rejected by the new middleware after upgrade.
type APIToken struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`  // human-readable, operator-supplied
	Hash       string     `json:"-"`      // sha256 hex; NEVER in JSON responses
	Scopes     []string   `json:"scopes"` // empty = legacy full-access token
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`

	// ExpiresAt is an optional expiry. When non-nil and in the past,
	// the middleware treats the token the same as revoked: validation
	// returns nil and the request gets a 401. Nil = never expires.
	// Setting expiries is encouraged but not required; the original
	// tokens issued before v0.11 have ExpiresAt=nil and stay valid
	// until explicitly revoked.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// RolloutState is the lifecycle position of a Rollout.
type RolloutState string

const (
	RolloutStatePending         RolloutState = "pending"          // created but engine hasn't picked it up yet
	RolloutStateInProgress      RolloutState = "in_progress"      // actively advancing through stages
	RolloutStatePaused          RolloutState = "paused"           // operator paused — engine no-ops, no advance, no auto-abort
	RolloutStateSucceeded       RolloutState = "succeeded"        // final stage completed cleanly
	RolloutStateAborted         RolloutState = "aborted"          // operator clicked Abort or criteria fired; rollback in progress
	RolloutStateRolledBack      RolloutState = "rolled_back"      // previous config restored; terminal
	RolloutStatePendingApproval RolloutState = "pending_approval" // v0.47 — created with require_approval=true; waiting for approver
	RolloutStateRejected        RolloutState = "rejected"         // v0.47 — approver rejected; terminal
)

// RolloutStageMode controls how the engine picks the canary set for a
// stage. "percent" is the original behavior — take the first N% of the
// group's agents in deterministic id order. "label" uses a key=value
// equality match against agent labels, letting operators name specific
// agents (e.g. host.name=canary-1) or whole sub-environments
// (e.g. deployment.environment=staging) as the canary.
type RolloutStageMode string

const (
	RolloutStageModePercent RolloutStageMode = "percent"
	RolloutStageModeLabel   RolloutStageMode = "label"
)

// RolloutStage is one promotion step. The Mode field decides which other
// fields are honored:
//   - "percent": Percentage (1-100). Cumulative — stage[N] targets that
//     many percent of the group's agents (so [10, 50, 100] means 10%
//     first, then expand to 50%, then 100%).
//   - "label": LabelSelector. AND-semantics over key=value equality on
//     agent labels. The matched set is the canary for this stage.
//     Stages within a label-mode rollout don't have a "cumulative"
//     constraint — the operator is responsible for ordering them in a
//     sensible superset progression.
//
// v1 requires every stage in a rollout to share the same mode. Mixed-mode
// rollouts return a validation error.
type RolloutStage struct {
	Mode          RolloutStageMode  `json:"mode"`                     // "percent" or "label"
	Percentage    int               `json:"percentage,omitempty"`     // for percent mode; 1-100
	LabelSelector map[string]string `json:"label_selector,omitempty"` // for label mode
	DwellSeconds  int               `json:"dwell_seconds"`            // pause at this stage before auto-advancing
}

// RolloutAbortCriteria are the conditions under which the engine auto-aborts
// a rollout and rolls back to PreviousConfigID. Conservative defaults are
// recommended — a rolled-back-by-mistake rollout is recoverable, a let-it-
// burn rollout often isn't.
type RolloutAbortCriteria struct {
	// MaxDriftedAgents: if more than this many canary agents end up in
	// drift state during a dwell, abort. 0 means any drift aborts.
	MaxDriftedAgents int `json:"max_drifted_agents"`

	// MaxErrorLogsPerMinute: if the canary agents collectively produce
	// more than this many ERROR/FATAL log records per minute (averaged
	// over the dwell window so far), abort. 0 disables the check.
	MaxErrorLogsPerMinute int `json:"max_error_logs_per_minute,omitempty"`

	// MinDwellSecondsBeforeAbort: how long after a stage starts the
	// engine waits before applying error-rate criteria. Gives newly-
	// pushed agents time to flush startup noise. Default 30s.
	MinDwellSecondsBeforeAbort int `json:"min_dwell_seconds_before_abort,omitempty"`
}

// Rollout is one safe staged config rollout against a group.
type Rollout struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	GroupID          string               `json:"group_id"`
	TargetConfigID   string               `json:"target_config_id"`
	PreviousConfigID string               `json:"previous_config_id,omitempty"` // captured at create time for rollback
	Stages           []RolloutStage       `json:"stages"`
	AbortCriteria    RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL  string               `json:"notification_url,omitempty"` // optional webhook for state transitions

	State          RolloutState `json:"state"`
	CurrentStage   int          `json:"current_stage"`              // index into Stages
	StageStartedAt *time.Time   `json:"stage_started_at,omitempty"` // when CurrentStage began dwelling
	AbortReason    string       `json:"abort_reason,omitempty"`     // populated when State transitions to aborted

	// v0.47 approval workflow. RequireApproval is set at create time;
	// when true the rollout starts in state "pending_approval" and
	// the engine refuses to advance it until Approve transitions it
	// to "pending". RequestedBy / ApprovedBy enforce the two-person
	// rule.
	RequireApproval bool       `json:"require_approval,omitempty"`
	RequestedBy     string     `json:"requested_by,omitempty"`
	ApprovedBy      string     `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time `json:"approved_at,omitempty"`
	RejectedBy      string     `json:"rejected_by,omitempty"`
	RejectedAt      *time.Time `json:"rejected_at,omitempty"`
	ApprovalNotes   string     `json:"approval_notes,omitempty"`

	// v0.49 — last blackout the engine hit on this rollout. Set
	// transiently when a tick refuses to advance because the
	// target group has an active change window; cleared on the
	// next successful advancement so the UI badge disappears
	// when the window closes.
	LastBlackoutReason string     `json:"last_blackout_reason,omitempty"`
	LastBlackoutAt     *time.Time `json:"last_blackout_at,omitempty"`

	// v0.53 — proposal provenance. Every rollout is conceptually a
	// proposal; ProposedBy records who or what originated it. The
	// open core's existing path defaults to "operator" so behavior
	// is unchanged from prior versions. The AI proposer pipeline
	// (Squadron Move 1) sets "ai" and populates ProposalReasoning
	// with the model's natural-language justification plus
	// EvidenceRefs with the alerts and metrics that informed the
	// proposal. The values flow through to the audit trail so the
	// compliance evidence path is consistent across operator,
	// AI, and system-originated changes.
	//
	// ProposedBy MUST be one of: "operator", "ai", "system".
	// Service-layer validation enforces this; storage allows any
	// string for forward compatibility with future origins.
	ProposedBy        string               `json:"proposed_by,omitempty"`
	ProposalReasoning string               `json:"proposal_reasoning,omitempty"`
	EvidenceRefs      []RolloutEvidenceRef `json:"evidence_refs,omitempty"`

	// v0.60 — operator initiated rollback. When this rollout was
	// created by clicking "Roll back" on a previous rollout, this
	// field carries the source rollout's ID. The UI uses it to render
	// a "rollback of <X>" badge and the audit timeline chains the
	// two rollouts together. Empty for normal rollouts.
	RolledBackFromID string `json:"rolled_back_from_id,omitempty"`

	// v0.69 — multi step plans. Groups rollouts that belong to one
	// approved plan. When the AI proposer recommends a fix that
	// requires multiple sequenced rollouts (e.g. "drop the noisy
	// attribute, then rotate the Splunk index, then update the
	// alert rule"), each rollout carries the same PlanID and a
	// PlanStepIndex (0, 1, 2…) that orders them. A single approval
	// on PlanStepIndex=0 gates the whole chain; the engine advances
	// step N+1 only after step N reaches succeeded. Empty PlanID
	// means a standalone rollout (the v0.4–v0.68 default), preserving
	// full backwards compatibility. See docs/multi-step-plans-design.md.
	// v0.82 — dropped omitempty on PlanStepIndex; 0 is the first
	// forward step and omitempty was hiding it on the wire (#543).
	PlanID        string `json:"plan_id,omitempty"`
	PlanStepIndex int    `json:"plan_step_index"`

	// v0.89.14 (#630) — action runner steps in plans, slice 1.
	// StepKind distinguishes "rollout" (the v0.4–v0.89.13 default,
	// staged config push) from "action" (a signed action-runner
	// verb dispatched mid-plan). Empty string decodes as "rollout"
	// for backwards compatibility with every existing row. When
	// StepKind=="action", ActionRequestID holds the ID of the
	// action_requests row the plan engine dispatched on the
	// predecessor's succeeded transition. ActionRequestID is empty
	// on every rollout step and on action steps that haven't been
	// dispatched yet (Queued). See docs/proposals/530-action-runner
	// -steps-in-plans.md for the protocol.
	StepKind        string `json:"step_kind,omitempty"`
	ActionRequestID string `json:"action_request_id,omitempty"`

	// v0.89.26 (#642) — per-rollout opt-out for the proposer-learns-
	// from-verdicts loop (#531 slice 2 §10 Q3). When true,
	// Bridge.assembleVerdicts skips this rollout when assembling the
	// few-shot examples block on the next AI proposal for the same
	// group. The group-level Group.LearnFromVerdicts flag (v0.89.17)
	// still short-circuits before this filter — group off ⇒ no
	// examples regardless of any per-rollout flag.
	//
	// Threat model this closes: an operator's typed rejection note
	// (ApprovalNotes) on an AI proposal containing PII, customer
	// names, or internal incident context would otherwise flow into
	// the next AI proposal's prompt verbatim. Setting this flag
	// suppresses the row entirely from the few-shot block without
	// flipping the whole group's loop off. Default false at the
	// storage layer; the schema v5 migration backfills every
	// existing row to 0 so post-upgrade behavior matches the design.
	ExcludeFromLearning bool `json:"exclude_from_learning,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"` // set on terminal state
}

// Rollout step kinds. v0.89.14 (#630). Stored in Rollout.StepKind.
// "" and StepKindRollout are equivalent on the wire so pre-v0.89.14
// rows round trip cleanly.
const (
	StepKindRollout = "rollout"
	StepKindAction  = "action"
)

// RolloutEvidenceRef is a single piece of evidence attached to a
// proposal. v0.53. Used by AI proposers to point at the alerts,
// metrics, configlint findings, or recommendations that informed
// the proposal. Stored as JSON on the rollouts row to avoid a
// second table for a piece of data the operator and the audit
// trail consume as one unit.
//
// Kind values: "alert", "metric", "configlint", "recommendation",
// "audit_event", "url". Open-ended on purpose so future evidence
// sources can plug in without a schema migration.
type RolloutEvidenceRef struct {
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
}

// Rollout proposal origins. Use these constants when constructing
// or comparing ProposedBy values so typos surface at compile time.
const (
	RolloutProposedByOperator = "operator"
	RolloutProposedByAI       = "ai"
	RolloutProposedBySystem   = "system"
)

// RolloutFilter narrows ListRollouts. Empty filter returns all.
type RolloutFilter struct {
	GroupID string
	State   RolloutState
	Limit   int
	// v0.74 — narrow to a single plan. Empty matches everything
	// (preserving v0.4–v0.73 behavior). Negative PlanStepIndex
	// rollback rollouts share the same PlanID, so a filtered query
	// returns the full forward + backward arc.
	PlanID string
}

// AuditEvent is one entry in the audit log. Every state change in Squadron
// — config push, drift transition, rule edit, agent registration — is
// recorded as an AuditEvent so operators have an answerable history when
// something goes wrong.
//
// The Payload is intentionally freeform so publishers can attach event-
// specific metadata (before/after, diff, value at firing, etc.) without
// forcing a schema migration every time we add a new event type.
type AuditEvent struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`           // when the event happened
	Actor      string         `json:"actor"`               // "system" | "operator:<email>" | "agent:<id>" | "opamp"
	EventType  string         `json:"event_type"`          // dotted name, e.g. "config.applied"
	TargetType string         `json:"target_type"`         // "agent" | "group" | "config" | "rule"
	TargetID   string         `json:"target_id,omitempty"` // affected entity id; may be empty for fleet-wide events
	Action     string         `json:"action"`              // "created" | "updated" | "deleted" | "applied" | "drift" | ...
	Payload    map[string]any `json:"payload,omitempty"`   // freeform JSON metadata
	CreatedAt  time.Time      `json:"created_at"`          // when the row was inserted

	// v0.57 — cached AI explanation of this audit row. Populated lazily
	// the first time an operator clicks "Explain" on the row in the UI.
	// Audit rows are immutable so a cached explanation never goes stale
	// in the data sense; operators can still force a refresh via the
	// regenerate endpoint when they want a different angle.
	AIExplanation            string     `json:"ai_explanation,omitempty"`
	AIExplanationModel       string     `json:"ai_explanation_model,omitempty"`
	AIExplanationGeneratedAt *time.Time `json:"ai_explanation_generated_at,omitempty"`
}

// AuditEventFilter narrows a ListAuditEvents query.
//
// All fields are optional. An empty filter returns the most recent events
// across the whole fleet (subject to Limit).
type AuditEventFilter struct {
	EventType  string // exact-match on dotted event_type; empty disables the filter
	TargetType string
	TargetID   string
	Since      time.Time // events with Timestamp >= Since; zero value disables the filter
	Limit      int       // default 100 if zero; capped at 1000 by the storage layer
}

// DiscoveryVerdict — v0.89.36 (#655 Stream 53, #531 slice 2 chunk 3)
// — minimal projection over the recommendation.pr_merged AND
// recommendation.pr_closed_not_merged audit_events rows that
// ListDiscoveryVerdicts returns. Renamed from AcceptedRecommendation
// (v0.89.28 #643 slice 1) and widened with a State discriminator so
// the discovery bridge can project both PR-outcome event types into
// the shared verdictsel.Verdict shape.
//
// Field-name discrepancy across States — documented intentionally:
//
//   - State="merged" rows (recommendation.pr_merged): PRMergedAt
//     carries the PR's merged_at timestamp; MergedBy carries the
//     login of whoever merged the PR.
//   - State="closed_not_merged" rows
//     (recommendation.pr_closed_not_merged): PRMergedAt carries the
//     PR's closed_at timestamp; MergedBy carries the login of
//     whoever closed the PR. The struct field names are kept
//     stable to avoid a v0.89.28 callsite churn that has zero
//     functional benefit at this layer — verdictprompt.Render
//     reads them only as Verdict.Timestamp and Verdict.Body and
//     surfaces them with state-correct wording ("merged 3 days
//     ago" vs "closed yesterday").
type DiscoveryVerdict struct {
	State              string    // "merged" or "closed_not_merged"; verdictsel State* constants
	PRMergedAt         time.Time // merged_at OR closed_at depending on State
	PRURL              string
	Branch             string
	MergedBy           string // merged_by OR closed_by depending on State
	RecommendationKind string

	// Provider + ScopeID identify the ORIGIN cloud of a cross-scope
	// verdict (cross-cloud citations). Empty on same-scope rows from
	// ListDiscoveryVerdicts; populated by ListCrossScopeDiscoveryVerdicts
	// (Provider in {aws,gcp,azure,oci}; ScopeID = the account / project /
	// subscription / tenancy the verdict was recorded against). The
	// discovery bridge renders these as the citation's origin label.
	Provider string
	ScopeID  string
}

// ExcludedRecommendation — v0.89.37 (#656 Stream 54, #531 slice 2
// chunk 4) — projection over one iac_recommendation_verdicts row.
// Carries the operator-set "Don't propose this again" verdict for
// a single discovery recommendation. The bridge reads these via
// ListExcludedRecommendations and folds each into the verdictsel
// pool as a StateOperatorExcluded entry; the prompt block renders
// `[OPERATOR_EXCLUDED]` stanzas per §7.2.
//
// Field semantics:
//
//   - RecommendationID: the deterministic ID the discovery proposer
//     assigned when it emitted the recommendation. PK on the
//     underlying table; same row updated on subsequent toggles.
//   - ConnectionID/AccountID/Region: the scope tuple the bridge keys
//     its sweep on. Matches the §6 selection algorithm's "same
//     connection_id + account_id + region only" rule.
//   - RecommendationKind: the kind string the proposer emitted (e.g.
//     "rds-pi-em", "eks-observability-addon"). The prompt's
//     `kind=<kind>` line surfaces this verbatim.
//   - ResourceID: nullable. Empty string ("") in the projection
//     means the exclusion is kind-level — operator never wants this
//     kind proposed against this scope. Non-empty scopes the
//     exclusion to a specific resource (the §11 Q4 distinction the
//     prompt renderer surfaces with different instruction text in a
//     later chunk).
//   - ExcludedAt/ExcludedBy: the "as of" stamp populated on the
//     transition to excluded=true. Cleared (zero / "") when the
//     row's exclude_from_learning flag is back to false. The bridge
//     uses ExcludedAt as the Verdict.Timestamp; the §7.2
//     `reference: operator_excluded=<date>` line formats from it.
type ExcludedRecommendation struct {
	RecommendationID   string
	ConnectionID       string
	AccountID          string
	Region             string
	RecommendationKind string
	ResourceID         string
	ExcludedAt         time.Time
	ExcludedBy         string
}

// CheckRunRef — v0.89.42 (#662 Stream 60) — storage-layer mirror of
// the iac/github Checks API check-run identifier. Defined locally so
// the storage package never imports the iac/github client; the two
// types are field-compatible and the bridge will convert between
// them at the boundary in chunk 2.
//
// The four fields together are the durable addressable identity of
// one check run on one PR's head commit:
//
//   - Owner / Repo: the GitHub repo coordinates the check run lives
//     on. Persisted alongside the row to avoid a second join on read
//     (re-deriving from connection_id would add a round-trip to the
//     hot path that updates the check on PR merge / close).
//   - CheckID: the int64 GitHub assigns on the create POST. The PATCH
//     path keys exclusively on this value; we MUST store it.
//   - HeadSHA: the commit SHA the check run was created against.
//     §7.2 of the design doc names "force-pushed head SHA" as the
//     reason this matters — slice 1 stays pinned to the original SHA
//     even when GitHub's HEAD moves, so the stored SHA is ground
//     truth for "which commit did Squadron speak on."
//
// See docs/proposals/checks-api-back-signal.md §6 and §9.
type CheckRunRef struct {
	Owner   string
	Repo    string
	CheckID int64
	HeadSHA string
}

// AlertSeverity is the severity level attached to a firing alert.
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// ThresholdOperator is the comparison used between the query result and the
// threshold value. Mirrors the common Prometheus expression operators.
type ThresholdOperator string

const (
	ThresholdGreater        ThresholdOperator = ">"
	ThresholdGreaterOrEqual ThresholdOperator = ">="
	ThresholdLess           ThresholdOperator = "<"
	ThresholdLessOrEqual    ThresholdOperator = "<="
	ThresholdEqual          ThresholdOperator = "=="
	ThresholdNotEqual       ThresholdOperator = "!="
)

// AlertRule defines a periodically-evaluated Squadron QL query and what to do
// when its scalar result satisfies the threshold.
//
// Example: name "high drift rate", query "fleet_drift_status_drifted",
// operator ">", threshold 5, interval 60s, severity warning,
// webhook https://hooks.example.com/squadron.
type AlertRule struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Query             string            `json:"query"`
	ThresholdOperator ThresholdOperator `json:"threshold_operator"`
	ThresholdValue    float64           `json:"threshold_value"`
	IntervalSeconds   int               `json:"interval_seconds"`
	Severity          AlertSeverity     `json:"severity"`
	Enabled           bool              `json:"enabled"`
	WebhookURL        string            `json:"webhook_url,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// Agent represents an OpenTelemetry agent
type Agent struct {
	ID              uuid.UUID         `json:"id"`
	Name            string            `json:"name"`
	Labels          map[string]string `json:"labels"`
	Status          AgentStatus       `json:"status"`
	LastSeen        time.Time         `json:"last_seen"`
	GroupID         *string           `json:"group_id,omitempty"`
	GroupName       *string           `json:"group_name,omitempty"`
	Version         string            `json:"version"`
	Capabilities    []string          `json:"capabilities"`
	EffectiveConfig string            `json:"effective_config,omitempty"`
	// DiscoverySource records how Squadron first learned about this
	// agent. v0.36 introduces "otlp" for collectors that send
	// telemetry to Squadron but never open an OpAMP connection;
	// "opamp" is the back-compat default for everything else.
	// Telemetry-only agents are observable but not manageable —
	// the UI surfaces this distinction so operators know they
	// can't push config to them until they're brought under OpAMP.
	DiscoverySource string `json:"discovery_source,omitempty"`
	// v0.51 — soft-delete tombstone. When set, the agent is
	// considered decommissioned. Squadron retains the row indefinitely
	// for audit history (CIP-007-6 R4.3 / R4.4) but ListAgents
	// excludes tombstones by default. The decommissioning operator's
	// identity is captured in the agent.decommissioned audit event,
	// not on the row itself; correlate by (TargetID == agent ID,
	// EventType == "agent.decommissioned").
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// AgentStatus represents the status of an agent
type AgentStatus string

const (
	AgentStatusOnline  AgentStatus = "online"
	AgentStatusOffline AgentStatus = "offline"
	AgentStatusError   AgentStatus = "error"
)

// Group represents a group of agents
type Group struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	// v0.48 — when true, every rollout created against this group
	// is forced into pending_approval regardless of what the
	// requester sets on the rollout input. This is the actual
	// compliance control: it converts the requester-set
	// require_approval checkbox from honor system to enforced
	// policy. Set this on production-tier groups so operators
	// can't accidentally (or intentionally) ship to prod without
	// the two-person rule firing.
	RequireApproval bool `json:"require_approval"`
	// v0.61 — separate policy for operator initiated rollbacks. When
	// true, every rollback rollout (i.e. one created via the
	// /rollouts/:id/rollback endpoint) is forced into
	// pending_approval regardless of whether the source rollout
	// required approval. Lets compliance flag rollback as the more
	// dangerous operation (you are undoing a change that already
	// shipped to prod) without forcing approval on every fresh
	// rollout. Independent of RequireApproval: a group can require
	// approval on all rollouts and rollbacks, on rollbacks only, on
	// neither, or on rollouts only.
	RequireApprovalForRollback bool `json:"require_approval_for_rollback"`
	// v0.49 — change windows. Recurring blackout periods that block
	// rollout advancement during change-restricted times (utility
	// peak demand hours, storm-response windows, quarterly freezes).
	// Stored as a JSON-serialized blob so the operator can manage
	// the list as one unit. Stored as []byte at this layer to keep
	// types.Group cleanly serializable; the changewindow package
	// owns the higher-level Window struct.
	ChangeWindowsJSON string `json:"change_windows,omitempty"`
	// v0.89.17 (#633) — per-group opt-out for the proposer-learns-
	// from-verdicts loop. When true (default), Bridge.assembleVerdicts
	// pulls prior AI-originated approvals/rejections on this group
	// and feeds them back as few-shot examples on the next cost-spike
	// proposal. Flip to false to suppress every prior-verdict
	// example for this group — the prompt block is omitted entirely
	// and the proposal.created audit row carries an empty
	// verdict_examples_used array so SIEM consumers can still see the
	// proposal happened without learning context. Default true at
	// the storage layer; the migration in schema v4 backfills every
	// existing row to 1 so post-upgrade behavior matches the design.
	LearnFromVerdicts bool      `json:"learn_from_verdicts"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Config represents an agent configuration
type Config struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	AgentID    *uuid.UUID `json:"agent_id,omitempty"`
	GroupID    *string    `json:"group_id,omitempty"`
	ConfigHash string     `json:"config_hash"`
	Content    string     `json:"content"`
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ConfigFilter represents filters for listing configs
type ConfigFilter struct {
	AgentID *uuid.UUID
	GroupID *string
	Limit   int
}

// SavedQuery represents a saved Squadron QL query
type SavedQuery struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Query       string    `json:"query"`
	Tags        []string  `json:"tags"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CostSpikeEvent records a detected anomaly in the fleet's
// $/month projection. v0.29 cost-spike alerting writes one row
// per spike — open until the projection drops back below the
// warn threshold. AttributionJSON is a compact summary of which
// agents / attributes drove the spike, captured at fire time so
// the operator sees a stable picture even hours later when the
// live insights state has moved on.
//
// Severity transitions:
//
//	warn     — projection ≥ baseline × (1 + warn_pct)
//	critical — projection ≥ baseline × (1 + critical_pct)
//
// A spike that crosses from warn to critical is updated in place
// (peak_pct grows); no new row is created until the spike closes.
type CostSpikeEvent struct {
	ID                   string     `json:"id"`
	StartedAt            time.Time  `json:"started_at"`
	EndedAt              *time.Time `json:"ended_at,omitempty"`
	Severity             string     `json:"severity"` // "warn" | "critical"
	Signal               string     `json:"signal,omitempty"`
	BaselineMonthlyUSD   float64    `json:"baseline_monthly_usd"`
	PeakMonthlyUSD       float64    `json:"peak_monthly_usd"`
	PeakPctAboveBaseline float64    `json:"peak_pct_above_baseline"`
	// AttributionJSON is a tiny JSON object: { "top_agents":[…],
	// "top_attributes":[…] } populated at fire time. Stored as
	// string for forward-compat — the detector can extend the
	// shape without a migration.
	AttributionJSON string     `json:"attribution_json,omitempty"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy  string     `json:"acknowledged_by,omitempty"`
}

// CostSpikeFilter restricts ListCostSpikeEvents.
type CostSpikeFilter struct {
	// Status: "open" (ended_at IS NULL), "closed" (ended_at IS
	// NOT NULL), "all" (default). Anything else falls through to
	// "all".
	Status string
	Limit  int
}

// SiemDestination is one configured downstream SIEM (Splunk HEC,
// signed webhook). Mirrors siem.Destination but lives at the storage
// layer; the service layer translates between the two shapes.
//
// Type values match siem.DestinationType strings: "splunk_hec",
// "webhook". The dispatcher dispatches an Event to a destination if
// the event's type starts with any of the EventTypePrefixesJSON
// (empty = forward everything).
//
// Secret is encrypted at rest. Format: nonce(24) || ciphertext from
// internal/siem/secrets.go. Never returned in API responses; the API
// layer only exposes HasSecret to the UI.
//
// LastEventSentAt / LastError / LastErrorAt are operational
// telemetry the dispatcher writes via UpdateSiemDestinationStatus so
// the UI can show "last delivered 30s ago" / "401 unauthorized" at
// a glance.
//
// Added in v0.50 for compliance-grade audit retention.
type SiemDestination struct {
	ID                    string     `json:"id"`
	Name                  string     `json:"name"`
	Type                  string     `json:"type"`
	URL                   string     `json:"url"`
	Secret                []byte     `json:"-"` // ciphertext
	Enabled               bool       `json:"enabled"`
	EventTypePrefixesJSON string     `json:"event_type_prefixes_json,omitempty"`
	LastEventSentAt       *time.Time `json:"last_event_sent_at,omitempty"`
	LastError             string     `json:"last_error,omitempty"`
	LastErrorAt           *time.Time `json:"last_error_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// ActionRunnerRegistration is one installed squadron-action-runner
// daemon. Squadron persists this on first enrollment so it knows
// which runners exist, what they're allowed to do, and which
// public key to use for return-channel authentication (future
// work; the MVP uses HTTPS).
//
// CapabilitiesJSON stores the runner's declared capability list as
// raw JSON. The string-typed field keeps storage out of the
// internal/actions type dependency graph; services parse the JSON
// at use time.
//
// RevokedAt is operator-controlled. A revoked runner stays in the
// table for audit history but is excluded from action dispatch.
// Squadron refuses to send signed requests to a revoked runner.
//
// Added in v0.53 as part of Move 2 (the action runner).
type ActionRunnerRegistration struct {
	RunnerID         string     `json:"runner_id"`
	Hostname         string     `json:"hostname"`
	PublicKeyPEM     string     `json:"public_key_pem"`
	CapabilitiesJSON string     `json:"capabilities_json"`
	RegisteredAt     time.Time  `json:"registered_at"`
	LastSeenAt       time.Time  `json:"last_seen_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
}

// ActionRequest is one signed request Squadron dispatched to a
// runner, plus the runner's eventual result. Two rows exist per
// approved action: one with Phase="dry_run" (preview shown in the
// approval drawer) and one with Phase="execute" (the actual
// execution). Linking by ProposalID + Phase gives the UI everything
// it needs to render the full action lifecycle.
//
// Status values:
//   - "pending"  request sent, runner has not yet responded
//   - "success"  runner completed the phase successfully
//   - "failure"  runner attempted but failed (non-zero exit)
//   - "denied"   runner refused (signature, unknown type, out of policy)
//
// DeniedFor is populated only on Status="denied" and names the
// rejection category for audit clarity.
//
// Added in v0.53 as part of Move 2.
type ActionRequest struct {
	ID                  string     `json:"id"`
	ProposalID          string     `json:"proposal_id,omitempty"`
	RunnerID            string     `json:"runner_id"`
	ActionType          string     `json:"action_type"`
	ParametersJSON      string     `json:"parameters_json"`
	Signature           string     `json:"signature"`
	Phase               string     `json:"phase"`
	Status              string     `json:"status"`
	DeniedFor           string     `json:"denied_for,omitempty"`
	DryRunOutputJSON    string     `json:"dry_run_output_json,omitempty"`
	ExecutionOutputJSON string     `json:"execution_output_json,omitempty"`
	IssuedAt            time.Time  `json:"issued_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
}

// ActionRequestFilter narrows List queries.
type ActionRequestFilter struct {
	ProposalID string
	RunnerID   string
	Status     string // "", "pending", "success", "failure", "denied"
	Limit      int
}

// IncidentDraft is Move 3 of the engineer copilot roadmap: a draft
// of an incident postmortem ticket that Squadron writes after an
// action runs. The operator reviews, edits, and publishes through
// whatever ticketing system their team uses (or copies to
// clipboard). See docs/incident-drafter-design.md for the data flow
// and threat model.
//
// One draft per action is the default; the bridge dedups on
// ActionRequestID so flapping does not flood the inbox.
type IncidentDraft struct {
	ID               string    `json:"id"`
	ActionRequestID  string    `json:"action_request_id,omitempty"`
	RolloutID        string    `json:"rollout_id,omitempty"`
	Status           string    `json:"status"` // draft | published | dismissed
	Title            string    `json:"title"`
	BodyMarkdown     string    `json:"body_markdown"`
	DraftContentJSON string    `json:"draft_content_json,omitempty"`
	Provider         string    `json:"provider,omitempty"`     // clipboard | github | linear | jira | generic
	ExternalID       string    `json:"external_id,omitempty"`  // set on publish
	ExternalURL      string    `json:"external_url,omitempty"` // set on publish
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// IncidentDraftFilter narrows a List query.
type IncidentDraftFilter struct {
	ActionRequestID string
	RolloutID       string
	Status          string // "", "draft", "published", "dismissed"
	Limit           int
}
