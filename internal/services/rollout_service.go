// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"
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
type RolloutService interface {
	Create(ctx context.Context, input RolloutInput) (*Rollout, error)
	Get(ctx context.Context, id string) (*Rollout, error)
	List(ctx context.Context, filter RolloutFilter) ([]*Rollout, error)
	Abort(ctx context.Context, id, reason string) (*Rollout, error)
	Pause(ctx context.Context, id string) (*Rollout, error)
	Resume(ctx context.Context, id string) (*Rollout, error)

	// Persist is used by the engine to write back transitions discovered
	// during evaluation. Service-layer guard so the engine doesn't reach
	// into the application store directly.
	Persist(ctx context.Context, rollout *Rollout) error
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

// RolloutStage is one promotion step. Percentage is cumulative.
type RolloutStage struct {
	Percentage   int `json:"percentage"`
	DwellSeconds int `json:"dwell_seconds"`
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
