// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package cliapi

import "time"

// Agent mirrors the agents endpoint response. Only the fields the CLI
// renders or filters on are declared — anything extra in the server's
// response is harmlessly ignored at decode time.
type Agent struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels"`
	Status      string            `json:"status"`
	LastSeen    time.Time         `json:"last_seen"`
	GroupID     *string           `json:"group_id,omitempty"`
	GroupName   *string           `json:"group_name,omitempty"`
	Version     string            `json:"version"`
	DriftStatus string            `json:"drift_status"`
}

// AgentsResponse is the shape of GET /api/v1/agents. The map is keyed
// by agent ID — we flatten it in the command layer for tabular display.
type AgentsResponse struct {
	Agents       map[string]Agent `json:"agents"`
	TotalCount   int              `json:"totalCount"`
	ActiveCount  int              `json:"activeCount"`
	InactiveCount int             `json:"inactiveCount"`
}

// Group mirrors /api/v1/groups items.
type Group struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"created_at"`
}

type GroupsResponse struct {
	Groups []Group `json:"groups"`
}

// Config mirrors /api/v1/configs items.
type Config struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AgentID    *string   `json:"agent_id,omitempty"`
	GroupID    *string   `json:"group_id,omitempty"`
	ConfigHash string    `json:"config_hash"`
	Content    string    `json:"content"`
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
}

type ConfigsResponse struct {
	Configs []Config `json:"configs"`
}

// CreateConfigRequest is the body POST /api/v1/configs accepts when the
// CLI is creating a config from a YAML file.
type CreateConfigRequest struct {
	Name    string  `json:"name"`
	GroupID *string `json:"group_id,omitempty"`
	AgentID *string `json:"agent_id,omitempty"`
	Content string  `json:"content"`
}

// LintFinding mirrors configlint.Finding for the lint subcommand.
type LintFinding struct {
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Line     int    `json:"line,omitempty"`
	Path     string `json:"path,omitempty"`
}

type LintResponse struct {
	Findings []LintFinding `json:"findings"`
}

// Rollout mirrors services.Rollout. CurrentStage indexes into Stages.
type Rollout struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	GroupID          string         `json:"group_id"`
	TargetConfigID   string         `json:"target_config_id"`
	PreviousConfigID string         `json:"previous_config_id,omitempty"`
	Stages           []RolloutStage `json:"stages"`
	AbortCriteria    RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL  string         `json:"notification_url,omitempty"`
	State            string         `json:"state"`
	CurrentStage     int            `json:"current_stage"`
	StageStartedAt   *time.Time     `json:"stage_started_at,omitempty"`
	AbortReason      string         `json:"abort_reason,omitempty"`
	// v0.69 — multi step plan grouping. Empty PlanID means
	// standalone. Negative PlanStepIndex is reserved for v0.72
	// rollback steps within the same plan. v0.82 dropped omitempty
	// on PlanStepIndex because 0 is a meaningful value (the first
	// forward step) and omitempty was silently stripping it, which
	// surfaced as "Plan · step ?" in the UI for the head of every
	// plan (#543). PlanID keeps omitempty because empty there really
	// is the absence signal.
	PlanID        string `json:"plan_id,omitempty"`
	PlanStepIndex int    `json:"plan_step_index"`
	// v0.89.14 (#630) — action runner steps in plans, slice 1.
	// StepKind distinguishes "rollout" (default) from "action".
	// ActionRequestID links an action step to the dispatched
	// action_request row so squadronctl can fetch the underlying
	// runner-side details via /api/v1/actions/:id when present.
	StepKind        string `json:"step_kind,omitempty"`
	ActionRequestID string `json:"action_request_id,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	CompletedAt      *time.Time     `json:"completed_at,omitempty"`
}

type RolloutStage struct {
	Mode          string            `json:"mode,omitempty"`
	Percentage    int               `json:"percentage,omitempty"`
	LabelSelector map[string]string `json:"label_selector,omitempty"`
	DwellSeconds  int               `json:"dwell_seconds"`
}

type RolloutAbortCriteria struct {
	MaxDriftedAgents           int `json:"max_drifted_agents"`
	MaxErrorLogsPerMinute      int `json:"max_error_logs_per_minute,omitempty"`
	MinDwellSecondsBeforeAbort int `json:"min_dwell_seconds_before_abort,omitempty"`
}

type RolloutsResponse struct {
	Rollouts []Rollout `json:"rollouts"`
}

// RolloutInput is the body POST /api/v1/rollouts accepts.
type RolloutInput struct {
	Name            string               `json:"name"`
	GroupID         string               `json:"group_id"`
	TargetConfigID  string               `json:"target_config_id"`
	Stages          []RolloutStage       `json:"stages"`
	AbortCriteria   RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL string               `json:"notification_url,omitempty"`
	// v0.47 — when true the rollout requires two person approval.
	// On plan creation only step 0's flag is honored; the server
	// forces steps 1..N to false (plans approve as a unit).
	RequireApproval bool `json:"require_approval,omitempty"`
}

// Plan mirrors the v0.74 services.Plan envelope returned by
// GET /api/v1/rollouts/plans/:id.
type Plan struct {
	PlanID        string    `json:"plan_id"`
	GroupID       string    `json:"group_id"`
	StepCount     int       `json:"step_count"`
	State         string    `json:"state"`
	Steps         []Rollout `json:"steps"`
	RollbackSteps []Rollout `json:"rollback_steps,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// CreatePlanRequest is the body POST /api/v1/rollouts/plans accepts.
// Steps is an ordered list of N rollout intents that the server
// groups under a single plan id with PlanStepIndex assigned 0..N-1.
type CreatePlanRequest struct {
	Steps []RolloutInput `json:"steps"`
}

// CreatePlanResponse is what POST /plans returns: the assigned
// plan id plus the created steps in step-index order.
type CreatePlanResponse struct {
	PlanID string    `json:"plan_id"`
	Steps  []Rollout `json:"steps"`
	Count  int       `json:"count"`
}

// ListPlansResponse mirrors the v0.89.2 GET /api/v1/rollouts/plans
// wire shape. Count is the length of Plans, included so CI scripts
// can pick it off without recounting. v0.89.2 (#554, backfill of
// the v0.77 squadronctl plans subcommand).
type ListPlansResponse struct {
	Plans []Plan `json:"plans"`
	Count int    `json:"count"`
}

// RolloutTemplate mirrors the /rollout-recipes/templates response.
type RolloutTemplate struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Description   string               `json:"description"`
	WhenToUse     string               `json:"when_to_use"`
	DefaultName   string               `json:"default_name"`
	Stages        []RolloutStage       `json:"stages"`
	AbortCriteria RolloutAbortCriteria `json:"abort_criteria"`
}

type RolloutTemplatesResponse struct {
	Templates []RolloutTemplate `json:"templates"`
}

// RolloutPreview mirrors services.RolloutPreview.
type RolloutPreview struct {
	GroupID      string        `json:"group_id"`
	Current      *Config       `json:"current,omitempty"`
	Target       Config        `json:"target"`
	Diff         DiffResult    `json:"diff"`
	LintFindings []LintFinding `json:"lint_findings"`
}

type DiffResult struct {
	Unified   string `json:"unified"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Identical bool   `json:"identical"`
}

// AbortRequest is the body POST /api/v1/rollouts/:id/abort accepts.
type AbortRequest struct {
	Reason string `json:"reason"`
}

// AuditEvent mirrors services.AuditEvent.
type AuditEvent struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	Actor      string         `json:"actor"`
	EventType  string         `json:"event_type"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id,omitempty"`
	Action     string         `json:"action"`
	Payload    map[string]any `json:"payload,omitempty"`
}

type AuditResponse struct {
	Events []AuditEvent `json:"events"`
}

// IncidentDraft mirrors types.IncidentDraft. The CLI surfaces this
// for `squadronctl incidents list/view/dismiss/publish`.
type IncidentDraft struct {
	ID              string    `json:"id"`
	ActionRequestID string    `json:"action_request_id,omitempty"`
	RolloutID       string    `json:"rollout_id,omitempty"`
	Status          string    `json:"status"`
	Title           string    `json:"title"`
	BodyMarkdown    string    `json:"body_markdown"`
	Provider        string    `json:"provider,omitempty"`
	ExternalID      string    `json:"external_id,omitempty"`
	ExternalURL     string    `json:"external_url,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IncidentsResponse is the wire shape for /api/v1/incidents/drafts.
type IncidentsResponse struct {
	Drafts []IncidentDraft `json:"drafts"`
}

// APIToken mirrors services.APIToken. Plaintext is NEVER on this type.
// Scopes is empty for legacy pre-v0.10 tokens (treated as full-access
// by the middleware); explicit scopes for v0.10+ tokens.
type APIToken struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type TokensResponse struct {
	Tokens []APIToken `json:"tokens"`
}

// CreateTokenResponse is the body POST /api/v1/auth/tokens returns.
// The plaintext is ONLY ever in this response.
type CreateTokenResponse struct {
	Token     APIToken `json:"token"`
	Plaintext string   `json:"plaintext"`
}

// --- IaC GitHub connection wire shapes ----------------------------
//
// v0.89.8 (#617, Stream 22) — backfill of the CLI side for the
// connect-IaC-repo surface that shipped in v0.89.3 → v0.89.5.
// Field names and json tags mirror the snake_case wire used by
// internal/api/handlers/iac_github.go; the shape mirrors the
// TypeScript surface in ui/src/api/iacGithub.ts row-for-row.

// IaCGitHubPlacementEntry is one (provider, resource_kind) →
// file_path row of the placement map. The eight canonical kinds
// (see ui/src/data/iacGithubWizard.ts) are the source of truth;
// the CLI wizard pre-populates the same eight and lets operators
// fill or skip per row.
type IaCGitHubPlacementEntry struct {
	Provider     string `json:"provider"`
	ResourceKind string `json:"resource_kind"`
	FilePath     string `json:"file_path"`
}

// IaCGitHubConnection is one row of GET /api/v1/iac/github/connections.
// Mirrors the handler's iacGitHubConnectionRow shape.
type IaCGitHubConnection struct {
	ConnectionID       string                    `json:"connection_id"`
	Provider           string                    `json:"provider"`
	AuthKind           string                    `json:"auth_kind"`
	RepoFullName       string                    `json:"repo_full_name"`
	DefaultBranch      string                    `json:"default_branch"`
	RepoLayout         string                    `json:"repo_layout"`
	BranchPrefix       string                    `json:"branch_prefix,omitempty"`
	ReviewerTeamHandle string                    `json:"reviewer_team_handle,omitempty"`
	PlacementMap       []IaCGitHubPlacementEntry `json:"placement_map"`
	CreatedAt          time.Time                 `json:"created_at"`
}

// ListIaCGitHubConnectionsResponse is the wire shape for
// GET /api/v1/iac/github/connections.
type ListIaCGitHubConnectionsResponse struct {
	Connections []IaCGitHubConnection `json:"connections"`
}

// IaCGitHubValidateRequest is POST /api/v1/iac/github/validate body.
// Token is the GitHub PAT; the server never persists it on this call
// — validate is dry-run for the same wire shape SaveConnection uses.
type IaCGitHubValidateRequest struct {
	Token         string                    `json:"token"`
	RepoFullName  string                    `json:"repo_full_name"`
	DefaultBranch string                    `json:"default_branch,omitempty"`
	PlacementMap  []IaCGitHubPlacementEntry `json:"placement_map"`
}

// IaCGitHubPreflightResult is one row of the validate response. Err
// is non-nil iff the per-row check failed (path not reachable, file
// missing on a non-default branch, etc); zero value means OK.
type IaCGitHubPreflightResult struct {
	Provider     string             `json:"provider"`
	ResourceKind string             `json:"resource_kind"`
	FilePath     string             `json:"file_path"`
	Exists       bool               `json:"exists"`
	ShaShort     string             `json:"sha_short,omitempty"`
	Err          *IaCHumanizedError `json:"err,omitempty"`
}

// IaCGitHubValidateResponse is POST /api/v1/iac/github/validate body.
// RepoErr is set when the validate failed at the repo-level (auth,
// 404, rate limit) before any per-row preflight ran.
type IaCGitHubValidateResponse struct {
	RepoFullName     string                     `json:"repo_full_name"`
	DefaultBranch    string                     `json:"default_branch"`
	RepoErr          *IaCHumanizedError         `json:"repo_err,omitempty"`
	PreflightResults []IaCGitHubPreflightResult `json:"preflight_results"`
	Errors           []IaCHumanizedError        `json:"errors,omitempty"`
}

// IaCGitHubSaveConnectionRequest is POST /api/v1/iac/github/connections
// body. Same Token + PlacementMap shape as validate, plus the
// persistence-only fields (RepoLayout, BranchPrefix, ReviewerTeamHandle).
type IaCGitHubSaveConnectionRequest struct {
	Token              string                    `json:"token"`
	RepoFullName       string                    `json:"repo_full_name"`
	DefaultBranch      string                    `json:"default_branch,omitempty"`
	RepoLayout         string                    `json:"repo_layout"`
	BranchPrefix       string                    `json:"branch_prefix,omitempty"`
	ReviewerTeamHandle string                    `json:"reviewer_team_handle,omitempty"`
	PlacementMap       []IaCGitHubPlacementEntry `json:"placement_map"`
}

// IaCGitHubSaveConnectionResponse is the success body of
// POST /api/v1/iac/github/connections.
type IaCGitHubSaveConnectionResponse struct {
	ConnectionID string `json:"connection_id"`
	RepoFullName string `json:"repo_full_name"`
	Status       string `json:"status"`
}

// IaCHumanizedError mirrors scanner.HumanizedError on the wire. The
// IaC handlers wrap it in gin.H{"error": HumanizedError}; the cliapi
// Client recognises that envelope on non-2xx and surfaces the
// message + suggested_step + doc_link to the operator.
type IaCHumanizedError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	SuggestedStep string `json:"suggested_step,omitempty"`
	DocLink       string `json:"doc_link,omitempty"`
}

// --- IaC GitHub open-pr + update-placement wire shapes ------------
//
// v0.89.15 (#631, Stream 32) — backfills the two subcommands the
// v0.89.8 (#617) Stream 22 slice deferred. The shapes mirror
// internal/api/handlers/iac_github.go's iacGitHubOpenPRRequest /
// iacGitHubOpenPRResponse (extended with the v0.89.12 #628 slice-2
// disposition_actual + lifecycle_ignored + hcl_patch_failure_reason
// fields) and iacGitHubUpdatePlacementMapRequest (the v0.89.4 #610
// PATCH endpoint).
//
// HCL patch payload is intentionally decoded as a generic raw object
// (json.RawMessage) on this side — the CLI relays whatever the
// proposer emitted to the handler verbatim, and the handler owns the
// structural parse via internal/iac/hclpatch.Patch. Keeping it
// opaque on the CLI side means a future hclpatch.Patch field change
// doesn't ripple into this package.

// IaCGitHubOpenPRRequest is POST
// /api/v1/iac/github/connections/:id/open-pr body. ScanID + StepIdx
// pin down the recommendation; ResourceKind keys the connection's
// placement_map row; Snippet is the proposer's Terraform; HCLPatch
// is the optional v0.89.12 structured patch payload (opaque to the
// CLI — the handler owns the parse).
type IaCGitHubOpenPRRequest struct {
	ScanID            string   `json:"scan_id"`
	StepIdx           int      `json:"step_idx"`
	ResourceKind      string   `json:"resource_kind"`
	Snippet           string   `json:"snippet"`
	ProposerReasoning string   `json:"proposer_reasoning,omitempty"`
	AffectedResources []string `json:"affected_resources,omitempty"`
	AccountID         string   `json:"account_id,omitempty"`
	// HCLPatch is decoded as a generic any so the CLI can relay any
	// shape the handler accepts without re-declaring hclpatch.Patch
	// here. A nil/missing value triggers the slice-1.5 append-only
	// fallback on the server side.
	HCLPatch map[string]any `json:"hcl_patch,omitempty"`
}

// IaCGitHubOpenPRResponse is the success body of POST
// /api/v1/iac/github/connections/:id/open-pr. DispositionActual +
// LifecycleIgnored + HCLPatchFailureReason are the v0.89.12 #628
// slice-2 additions; older servers leave them empty.
type IaCGitHubOpenPRResponse struct {
	PRNumber              int    `json:"pr_number"`
	PRURL                 string `json:"pr_url"`
	Branch                string `json:"branch"`
	CommitSHA             string `json:"commit_sha"`
	FilePath              string `json:"file_path"`
	RepoFullName          string `json:"repo_full_name"`
	Disposition           string `json:"disposition,omitempty"`
	ManualMergeRequired   bool   `json:"manual_merge_required,omitempty"`
	DispositionActual     string `json:"disposition_actual,omitempty"`
	LifecycleIgnored      bool   `json:"lifecycle_ignored,omitempty"`
	HCLPatchFailureReason string `json:"hcl_patch_failure_reason,omitempty"`
}

// IaCGitHubUpdatePlacementMapRequest is PATCH
// /api/v1/iac/github/connections/:id/placement-map body. Only the
// placement_map is mutable through this endpoint — token / repo /
// branch_prefix / reviewer_team_handle stay owned by the connect
// wizard's create path.
type IaCGitHubUpdatePlacementMapRequest struct {
	PlacementMap []IaCGitHubPlacementEntry `json:"placement_map"`
}

// IaCGitHubUpdatePlacementMapResponse is the success body of PATCH
// /api/v1/iac/github/connections/:id/placement-map. Echoes back the
// connection metadata + the persisted placement map so callers can
// confirm the post-write shape without a follow-up GET.
type IaCGitHubUpdatePlacementMapResponse struct {
	ConnectionID string                    `json:"connection_id"`
	RepoFullName string                    `json:"repo_full_name"`
	PlacementMap []IaCGitHubPlacementEntry `json:"placement_map"`
}

// --- AWS scan-all wire shapes -------------------------------------
//
// v0.89.7a (#616, Stream 21) added the multi-account scan-all
// endpoint. v0.89.8 surfaces it on the CLI as
// `squadronctl discovery aws scan-all`. Fields mirror
// internal/api/handlers/discovery.go's awsScanAllResponse row-for-row.

// AWSScanAllAccountRow is one succeeded-account row of the
// scan-all response.
type AWSScanAllAccountRow struct {
	AccountID           string `json:"account_id"`
	ScanID              string `json:"scan_id"`
	ResourceCount       int    `json:"resource_count"`
	InstrumentedCount   int    `json:"instrumented_count"`
	UninstrumentedCount int    `json:"uninstrumented_count"`
}

// AWSScanAllFailureRow is one failed-account row. ErrorCode is the
// stable identifier callers can branch on; HumanizedMessage is the
// operator-facing prose. Neither field ever carries credential bytes.
type AWSScanAllFailureRow struct {
	AccountID        string `json:"account_id"`
	ErrorCode        string `json:"error_code"`
	HumanizedMessage string `json:"humanized_message"`
}

// AWSScanAllResponse is the wire shape returned by
// POST /api/v1/discovery/aws/scan-all.
type AWSScanAllResponse struct {
	ScanAllID           string                 `json:"scan_all_id"`
	TotalAccounts       int                    `json:"total_accounts"`
	SucceededAccounts   []AWSScanAllAccountRow `json:"succeeded_accounts"`
	FailedAccounts      []AWSScanAllFailureRow `json:"failed_accounts"`
	TotalResources      int                    `json:"total_resources"`
	TotalInstrumented   int                    `json:"total_instrumented"`
	TotalUninstrumented int                    `json:"total_uninstrumented"`
	Partial             bool                   `json:"partial"`
	Concurrency         int                    `json:"concurrency"`
}
