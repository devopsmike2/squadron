// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/configdiff"
	"github.com/devopsmike2/squadron/internal/configlint"
)

// RolloutService manages the lifecycle of safe staged config rollouts.
//
// Create persists a new rollout in the pending state — the background
// engine picks it up and advances it through its stages, watching the
// canary's drift state to decide whether to advance or abort.
//
// Abort flips an in-progress rollout to the aborted state; the engine
// performs the actual rollback (pushing the previous config back to the
// affected agents) and transitions to rolled_back.
//
// Preview is a read-only helper: given a target group and target
// config, it returns what shipping this rollout would actually do —
// the live config the group is on right now, the target the operator
// picked, a line-level diff, and lint findings against the target.
// The UI calls this before the operator clicks Start so the diff lands
// in front of them at the most useful moment.
type RolloutService interface {
	Create(ctx context.Context, input RolloutInput) (*Rollout, error)
	Get(ctx context.Context, id string) (*Rollout, error)
	List(ctx context.Context, filter RolloutFilter) ([]*Rollout, error)
	Abort(ctx context.Context, id, reason string) (*Rollout, error)
	Pause(ctx context.Context, id string) (*Rollout, error)
	Resume(ctx context.Context, id string) (*Rollout, error)
	Preview(ctx context.Context, groupID, targetConfigID string) (*RolloutPreview, error)

	// v0.47 — approval workflow.
	// Approve transitions a rollout from pending_approval to
	// pending (which the engine then advances). approver must not
	// equal the rollout's RequestedBy; ErrSelfApproval otherwise.
	Approve(ctx context.Context, id, approver, notes string) (*Rollout, error)
	// Reject is a terminal transition — the requester has to clone
	// the rollout to retry.
	Reject(ctx context.Context, id, rejecter, notes string) (*Rollout, error)

	// v0.60 — operator initiated rollback. RollBack creates a new
	// rollout that targets the source rollout's PreviousConfigID
	// (the config the group was on before the source ran) and
	// links back via RolledBackFromID. The source must be in a
	// terminal state (succeeded or aborted). Operators reach for
	// this when a completed rollout looked fine at the time but is
	// degrading metrics now; one click creates a clean new rollout
	// to undo it that goes through the same approval and audit
	// pipeline as any other rollout.
	RollBack(ctx context.Context, id, operator string) (*Rollout, error)

	// Persist is used by the engine to write back transitions discovered
	// during evaluation. Service-layer guard so the engine doesn't reach
	// into the application store directly.
	Persist(ctx context.Context, rollout *Rollout) error

	// v0.70 — multi step plan support. NextPlanStep looks up the
	// rollout with (PlanID = planID, PlanStepIndex = currentIndex+1).
	// Returns (nil, nil) when there is no next step (currentIndex was
	// the final step in the plan), letting the caller emit
	// plan.completed. The engine calls this from finish() to promote
	// the next step out of Queued; see docs/multi-step-plans-design.md
	// for the protocol.
	NextPlanStep(ctx context.Context, planID string, currentIndex int) (*Rollout, error)

	// v0.71 — cancellation walk. Find every queued step in planID
	// with index strictly greater than afterIndex and transition
	// each to Cancelled. Returns the list of cancelled rollouts so
	// the caller can emit per step audit events. The engine calls
	// this from triggerAbort and Reject's plan branch; the
	// transition is a no op for plans with no queued followers
	// (the failed step was the last in the plan).
	CancelPlanFollowers(ctx context.Context, planID string, afterIndex int) ([]*Rollout, error)

	// v0.72 — backwards rollback walk. When a plan step fails,
	// every succeeded forward step (index 0..failedIndex-1) needs
	// its config undone or the collectors are left running the
	// partial change. RollBackPlanPredecessors finds the succeeded
	// forward steps and creates a rollback rollout for each, using
	// the reserved negative PlanStepIndex range so the timeline
	// can distinguish rollback steps from forward steps.
	//
	// Returns the rollback rollouts in creation order (highest
	// forward step's rollback first, step 0's last). Empty slice
	// means there were no succeeded forward steps to roll back —
	// e.g. step 0 itself aborted, no work to do.
	RollBackPlanPredecessors(ctx context.Context, planID string, failedIndex int, operator string) ([]*Rollout, error)

	// v0.73 — plan create entry point. Wraps N Create calls under a
	// shared PlanID with PlanStepIndex assigned 0..N-1 in step
	// order. The first step's RequireApproval flag is honored (it's
	// the plan's approval gate); steps 1..N are forced to
	// RequireApproval=false because per the design doc the plan
	// approves as a unit at step 0.
	//
	// Partial failure: if step K's Create fails after K-1 already
	// succeeded, the implementation cancels the K-1 already-created
	// steps so the storage doesn't hold an orphan partial plan.
	// Returns the created steps and the assigned PlanID; the steps
	// are returned in step-index order so callers can render the
	// plan immediately.
	CreatePlan(ctx context.Context, steps []RolloutInput) ([]*Rollout, string, error)

	// v0.74 — plan read entry point. Returns the plan envelope: the
	// shared metadata plus the rollouts that make it up, separated
	// into forward steps (sorted by PlanStepIndex ascending) and
	// rollback steps (negative PlanStepIndex from v0.72, sorted
	// ascending so -1 comes first). Returns (nil, nil) when no
	// rollouts exist with this PlanID, so the handler can map to a
	// 404 without ambiguity. See docs/multi-step-plans-design.md.
	GetPlan(ctx context.Context, planID string) (*Plan, error)
}

// Plan is the envelope shape returned by GetPlan. v0.74.
// Plans are not a first-class storage entity (per the design doc),
// so this struct is computed from the underlying rollouts at read
// time; mutations happen on the rollouts directly via the existing
// rollout endpoints.
type Plan struct {
	// PlanID is the shared id every step in the plan carries.
	PlanID string `json:"plan_id"`

	// GroupID is taken from step 0 — every step in a plan shares
	// the same group, enforced at CreatePlan time.
	GroupID string `json:"group_id"`

	// StepCount is the number of forward steps in the plan
	// (ignoring rollback steps that have negative PlanStepIndex).
	StepCount int `json:"step_count"`

	// State summarizes the plan's lifecycle in one word. Derived
	// from the underlying rollouts:
	//   - pending_approval: step 0 is in PendingApproval
	//   - in_progress:      any step is InProgress, or step 0 is
	//                       Pending (engine hasn't picked it up yet)
	//   - succeeded:        every forward step is Succeeded
	//   - rejected:         step 0 is Rejected
	//   - cancelled:        any forward step is Cancelled
	//   - aborted:          any forward step is Aborted
	//   - rolled_back:      any rollback step exists
	//
	// The string is a derived view, not a stored value. Don't rely
	// on it as the canonical status for SIEM — use the per step
	// rollout.* and plan.* audit events for that.
	State string `json:"state"`

	// Steps is the forward steps in PlanStepIndex order. Step 0
	// is index 0 in the slice, step 1 is index 1, etc.
	Steps []*Rollout `json:"steps"`

	// RollbackSteps is the rollback steps in PlanStepIndex
	// ascending order (-1, -2, …). Empty when the plan hasn't
	// triggered backwards rollback. v0.72 created these.
	RollbackSteps []*Rollout `json:"rollback_steps,omitempty"`

	// CreatedAt is step 0's CreatedAt — the moment CreatePlan
	// committed the plan to storage. UpdatedAt is the most recent
	// timestamp across all steps so a UI showing "last activity"
	// gets the right value without having to walk the list.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RolloutPreview is the response shape of a Preview call.
//
// Current may be nil if the group has no current effective config (a
// brand-new group where this rollout will be the first push). The UI
// renders that as "everything is new" and the diff shows the entire
// target as +-lines.
//
// RolloutTracer is the slim contract the rollout service uses to fan
// lifecycle events out as OTel span events. Lives here (not as a
// direct reference to the rollouts.Tracer) because the rollouts
// package imports services, not the other way around — the real
// tracer satisfies this interface and gets injected via main.go.
//
// All methods MUST be nil-receiver-safe so service constructors can
// take a nil tracer and call unconditionally.
//
// RecordEvent attaches a named event to the rollout's active parent
// span. Used for pause/resume transitions today; future state changes
// that happen at service boundaries (rather than inside the engine)
// will reach for this same method.
//
// LinkRolloutToContext stores the caller's OTel span context so that
// when the engine eventually opens the rollout's parent span, it can
// add a link back to the originating API request. Spans live across
// many engine ticks while the API span ended seconds ago, so a true
// parent-child relationship doesn't fit; span links are the OTel-
// blessed primitive for "related but not parent-child". Operators
// viewing the API trace can navigate to the linked rollout trace and
// vice versa.
type RolloutTracer interface {
	RecordEvent(rolloutID, name, reason string)
	LinkRolloutToContext(rolloutID string, ctx context.Context)
}

// LintFindings is always non-nil so the UI can rely on .length without
// a null check.
type RolloutPreview struct {
	GroupID         string              `json:"group_id"`
	Current         *Config             `json:"current,omitempty"`
	Target          *Config             `json:"target"`
	Diff            configdiff.Result   `json:"diff"`
	LintFindings    []configlint.Finding `json:"lint_findings"`
}

// RolloutState mirrors applicationstore.RolloutState so consumers don't
// have to import the storage package.
type RolloutState string

const (
	RolloutStatePending    RolloutState = "pending"
	RolloutStateInProgress RolloutState = "in_progress"
	RolloutStatePaused     RolloutState = "paused"
	RolloutStateSucceeded  RolloutState = "succeeded"
	RolloutStateAborted    RolloutState = "aborted"
	RolloutStateRolledBack RolloutState = "rolled_back"
	// v0.47 — created with require_approval=true. The engine
	// refuses to advance from this state; an approver has to call
	// the Approve endpoint first, which transitions us to "pending"
	// and the normal lifecycle kicks in.
	RolloutStatePendingApproval RolloutState = "pending_approval"
	// v0.47 — terminal state when an approver explicitly rejects
	// the rollout. Engine ignores it; the requester can clone the
	// rollout with adjustments and re-submit.
	RolloutStateRejected RolloutState = "rejected"

	// v0.70 — multi step plan steps after the first sit in this
	// state until the previous step reaches succeeded. The engine
	// then promotes step N+1 from queued to pending, which the
	// normal tick loop picks up. Step 0 is created in pending (or
	// pending_approval) the same way a standalone rollout is — the
	// plan approval gate sits there. See
	// docs/multi-step-plans-design.md.
	RolloutStateQueued RolloutState = "queued"

	// v0.71 — terminal state for queued plan steps that never get
	// to run because an earlier step in the plan failed or was
	// rejected. The cancellation walk visits every queued step
	// with index > the failed step's and flips them to cancelled
	// in one pass. SIEM consumers see plan.step_cancelled per step
	// plus a plan level plan.rejected or plan.cancelled summary.
	RolloutStateCancelled RolloutState = "cancelled"
)

// RolloutStageMode mirrors applicationstore.RolloutStageMode.
type RolloutStageMode string

const (
	RolloutStageModePercent RolloutStageMode = "percent"
	RolloutStageModeLabel   RolloutStageMode = "label"
)

// RolloutStage is one promotion step. See applicationstore.RolloutStage for
// the full doc — in short, Mode picks the selection strategy and the
// matching field (Percentage for percent, LabelSelector for label).
type RolloutStage struct {
	Mode          RolloutStageMode  `json:"mode"`
	Percentage    int               `json:"percentage,omitempty"`
	LabelSelector map[string]string `json:"label_selector,omitempty"`
	DwellSeconds  int               `json:"dwell_seconds"`
}

// RolloutAbortCriteria — see applicationstore.RolloutAbortCriteria.
type RolloutAbortCriteria struct {
	MaxDriftedAgents           int `json:"max_drifted_agents"`
	MaxErrorLogsPerMinute      int `json:"max_error_logs_per_minute,omitempty"`
	MinDwellSecondsBeforeAbort int `json:"min_dwell_seconds_before_abort,omitempty"`
}

// Rollout is the service-layer view of an applicationstore.Rollout.
type Rollout struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	GroupID          string               `json:"group_id"`
	TargetConfigID   string               `json:"target_config_id"`
	PreviousConfigID string               `json:"previous_config_id,omitempty"`
	Stages           []RolloutStage       `json:"stages"`
	AbortCriteria    RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL  string               `json:"notification_url,omitempty"`

	State          RolloutState `json:"state"`
	CurrentStage   int          `json:"current_stage"`
	StageStartedAt *time.Time   `json:"stage_started_at,omitempty"`
	AbortReason    string       `json:"abort_reason,omitempty"`

	// v0.47 — approval workflow. When RequireApproval is true,
	// rollouts created via Create start in RolloutStatePendingApproval
	// and the engine refuses to advance them until an approver
	// transitions the state.
	RequireApproval bool       `json:"require_approval,omitempty"`
	RequestedBy     string     `json:"requested_by,omitempty"`
	ApprovedBy      string     `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time `json:"approved_at,omitempty"`
	RejectedBy      string     `json:"rejected_by,omitempty"`
	RejectedAt      *time.Time `json:"rejected_at,omitempty"`
	ApprovalNotes   string     `json:"approval_notes,omitempty"`

	// v0.49 — change-window enforcement. Set by the rollout engine
	// when a tick skips advancement because the target group has
	// an active blackout (peak demand hours, storm response, etc).
	// UI shows this on the rollout card so operators understand
	// why a rollout is sitting in pending or not advancing through
	// stages. Cleared automatically on the next successful
	// advancement so the badge disappears when the window closes.
	LastBlackoutReason string     `json:"last_blackout_reason,omitempty"`
	LastBlackoutAt     *time.Time `json:"last_blackout_at,omitempty"`

	// v0.53 — proposal provenance. Every rollout is a proposal.
	// ProposedBy records the origin: "operator" (default), "ai",
	// or "system". ProposalReasoning carries the natural-language
	// justification (used by AI proposers; usually empty for
	// operator-originated rollouts). EvidenceRefs points at the
	// alerts, metrics, configlint findings, or recommendations
	// that informed the proposal. The UI surfaces all three in
	// the approval drawer and audit log, and the SIEM fan-out
	// includes them in every audit event so external systems can
	// reconstruct the full chain. Squadron Move 1.
	ProposedBy        string         `json:"proposed_by,omitempty"`
	ProposalReasoning string         `json:"proposal_reasoning,omitempty"`
	EvidenceRefs      []EvidenceRef  `json:"evidence_refs,omitempty"`

	// v0.60 — operator initiated rollback. Set when this rollout was
	// created by clicking "Roll back" on a previous rollout. The
	// value is the source rollout's ID. UI uses it for the
	// "rollback of <X>" badge and the audit timeline chains the
	// two rollouts together.
	RolledBackFromID string `json:"rolled_back_from_id,omitempty"`

	// v0.69 — multi step plan grouping. Mirrors types.Rollout's
	// PlanID + PlanStepIndex. See docs/multi-step-plans-design.md
	// for the protocol. Empty PlanID is a standalone rollout. v0.82
	// — dropped omitempty on PlanStepIndex; 0 is meaningful (#543).
	PlanID        string `json:"plan_id,omitempty"`
	PlanStepIndex int    `json:"plan_step_index"`

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// EvidenceRef is one piece of evidence attached to a proposal. v0.53.
// Mirrors applicationstore/types.RolloutEvidenceRef so the service
// layer doesn't leak the storage type to handlers.
type EvidenceRef struct {
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
}

// Rollout proposal origins. Use these constants when constructing
// or comparing ProposedBy values. Mirrors the storage-layer
// constants so callers in services/ don't need to import
// applicationstore/types.
const (
	RolloutProposedByOperator = "operator"
	RolloutProposedByAI       = "ai"
	RolloutProposedBySystem   = "system"
)

// RolloutInput is the user-supplied shape Create accepts. The service
// derives ID, timestamps, and PreviousConfigID (snapshotting the group's
// current config so we have a rollback target).
type RolloutInput struct {
	Name            string               `json:"name"`
	GroupID         string               `json:"group_id"`
	TargetConfigID  string               `json:"target_config_id"`
	Stages          []RolloutStage       `json:"stages"`
	AbortCriteria   RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL string               `json:"notification_url"`
	// v0.47 — when true, the rollout enters pending_approval and
	// waits for an Approve call before the engine advances. A
	// second person must approve (the requester can't approve
	// their own rollout — enforced in ApproveRollout).
	RequireApproval bool `json:"require_approval,omitempty"`
	// v0.47 — auth actor of the request, populated by the handler
	// from the gin.Context. Stored as RequestedBy on the rollout
	// so the two-person rule can compare against it at approval
	// time. Empty in dev / token-less mode (the two-person rule
	// then matches on the audit actor placeholder).
	RequestedBy string `json:"-"`

	// v0.53 — proposal provenance. Most operator-originated rollouts
	// leave these empty; the service layer defaults ProposedBy to
	// "operator" at Create time. AI-originated proposals set
	// ProposedBy to "ai" plus a natural-language ProposalReasoning
	// and an EvidenceRefs slice pointing at the alerts / metrics /
	// recommendations that informed the proposal. The values flow
	// through the audit trail so the compliance evidence chain is
	// consistent across origins.
	ProposedBy        string        `json:"proposed_by,omitempty"`
	ProposalReasoning string        `json:"proposal_reasoning,omitempty"`
	EvidenceRefs      []EvidenceRef `json:"evidence_refs,omitempty"`

	// v0.69 — multi step plan grouping. When the proposer creates a
	// plan, every step shares the same PlanID and each step gets a
	// distinct PlanStepIndex. The engine sequencing that uses these
	// fields lands in v0.70+; for now Create just round trips them
	// to storage so the contract is stable. See
	// docs/multi-step-plans-design.md. v0.82 — dropped omitempty on
	// PlanStepIndex (consistency with the storage + response shapes
	// fixed in #543; omitempty doesn't affect decode anyway).
	PlanID        string `json:"plan_id,omitempty"`
	PlanStepIndex int    `json:"plan_step_index"`

	// v0.78 — inline config snippet. Used only by CreatePlan today:
	// when a plan step supplies this instead of TargetConfigID, the
	// server lints the YAML, materializes a new Config row in the
	// step's GroupID, and sets the step's TargetConfigID to the
	// new config's id before the rollout is persisted. Empty
	// snippet + non-empty TargetConfigID preserves the v0.4–v0.77
	// behavior of pointing at a pre-existing config.
	//
	// CreatePlan rejects steps where both are set (ambiguous) or
	// both are empty (no target). Regular Create (single rollout)
	// ignores this field; only the plan path interprets it. This
	// unblocks v0.79 proposer plan output — the model emits YAML
	// and the server materializes.
	InlineConfigSnippet string `json:"inline_config_snippet,omitempty"`
}

// RolloutFilter narrows List queries.
type RolloutFilter struct {
	GroupID string
	State   RolloutState
	Limit   int
	// v0.74 — narrow to a single plan. Empty matches everything.
	// Both forward steps and rollback steps (negative
	// PlanStepIndex) share the same PlanID, so a filtered query
	// returns the full forward + backward arc.
	PlanID string
}

// IsTerminal reports whether a rollout has reached an end state and the
// engine should ignore it. v0.71 — Cancelled joins the list because a
// cancelled plan step never ran, so there's no rollback work to do.
// Aborted and Rejected are NOT terminal by this definition because the
// engine still has rollback work to perform on aborted rollouts and the
// rejected state may still need cleanup.
func (s RolloutState) IsTerminal() bool {
	return s == RolloutStateSucceeded || s == RolloutStateRolledBack || s == RolloutStateCancelled
}
