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

	// Persist is used by the engine to write back transitions discovered
	// during evaluation. Service-layer guard so the engine doesn't reach
	// into the application store directly.
	Persist(ctx context.Context, rollout *Rollout) error
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

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

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
}

// RolloutFilter narrows List queries.
type RolloutFilter struct {
	GroupID string
	State   RolloutState
	Limit   int
}

// IsTerminal reports whether a rollout has reached an end state and the
// engine should ignore it.
func (s RolloutState) IsTerminal() bool {
	return s == RolloutStateSucceeded || s == RolloutStateRolledBack
}
