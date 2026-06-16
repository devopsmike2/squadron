package types

import (
	"context"
	"time"

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

	// Rollouts (safe staged config rollouts)
	CreateRollout(ctx context.Context, rollout *Rollout) error
	GetRollout(ctx context.Context, id string) (*Rollout, error)
	ListRollouts(ctx context.Context, filter RolloutFilter) ([]*Rollout, error)
	UpdateRollout(ctx context.Context, rollout *Rollout) error

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
//   pending      → just created, no observation yet
//   realized     → observed byte rate dropped below baseline; savings active
//   not_observed → 24h+ since apply, byte rate hasn't dropped (operator
//                  may have decided not to roll out, or the rollout
//                  hasn't reached the affected agents yet)
//   reverted     → observed byte rate is back near baseline AFTER once
//                  being realized (rollback or config drift)
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
	DismissedBy      string    `json:"dismissed_by"`        // actor identifier ("operator:email", "system", etc.)
	Reason           string    `json:"reason,omitempty"`    // optional free-text reason
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
	Label      string     `json:"label"`      // human-readable, operator-supplied
	Hash       string     `json:"-"`          // sha256 hex; NEVER in JSON responses
	Scopes     []string   `json:"scopes"`     // empty = legacy full-access token
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

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"` // set on terminal state
}

// RolloutFilter narrows ListRollouts. Empty filter returns all.
type RolloutFilter struct {
	GroupID string
	State   RolloutState
	Limit   int
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
	Timestamp  time.Time      `json:"timestamp"`            // when the event happened
	Actor      string         `json:"actor"`                // "system" | "operator:<email>" | "agent:<id>" | "opamp"
	EventType  string         `json:"event_type"`           // dotted name, e.g. "config.applied"
	TargetType string         `json:"target_type"`          // "agent" | "group" | "config" | "rule"
	TargetID   string         `json:"target_id,omitempty"`  // affected entity id; may be empty for fleet-wide events
	Action     string         `json:"action"`               // "created" | "updated" | "deleted" | "applied" | "drift" | ...
	Payload    map[string]any `json:"payload,omitempty"`    // freeform JSON metadata
	CreatedAt  time.Time      `json:"created_at"`           // when the row was inserted
}

// AuditEventFilter narrows a ListAuditEvents query.
//
// All fields are optional. An empty filter returns the most recent events
// across the whole fleet (subject to Limit).
type AuditEventFilter struct {
	TargetType string
	TargetID   string
	Since      time.Time // events with Timestamp >= Since; zero value disables the filter
	Limit      int       // default 100 if zero; capped at 1000 by the storage layer
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
	DiscoverySource string    `json:"discovery_source,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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
	// v0.49 — change windows. Recurring blackout periods that block
	// rollout advancement during change-restricted times (utility
	// peak demand hours, storm-response windows, quarterly freezes).
	// Stored as a JSON-serialized blob so the operator can manage
	// the list as one unit. Stored as []byte at this layer to keep
	// types.Group cleanly serializable; the changewindow package
	// owns the higher-level Window struct.
	ChangeWindowsJSON string    `json:"change_windows,omitempty"`
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
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Query       string   `json:"query"`
	Tags        []string `json:"tags"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
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
	ID                     string     `json:"id"`
	Name                   string     `json:"name"`
	Type                   string     `json:"type"`
	URL                    string     `json:"url"`
	Secret                 []byte     `json:"-"` // ciphertext
	Enabled                bool       `json:"enabled"`
	EventTypePrefixesJSON  string     `json:"event_type_prefixes_json,omitempty"`
	LastEventSentAt        *time.Time `json:"last_event_sent_at,omitempty"`
	LastError              string     `json:"last_error,omitempty"`
	LastErrorAt            *time.Time `json:"last_error_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}
