package applicationstore

// Re-export types from the types package for convenience
import "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"

// Type aliases for convenience
type ApplicationStore = types.ApplicationStore
type Agent = types.Agent
type AgentStatus = types.AgentStatus
type Group = types.Group
type Config = types.Config
type ConfigFilter = types.ConfigFilter
type SavedQuery = types.SavedQuery
type AlertRule = types.AlertRule
type AlertSeverity = types.AlertSeverity
type ThresholdOperator = types.ThresholdOperator
type AuditEvent = types.AuditEvent
type AuditEventFilter = types.AuditEventFilter
type Rollout = types.Rollout
type RolloutStage = types.RolloutStage
type RolloutStageMode = types.RolloutStageMode
type RolloutAbortCriteria = types.RolloutAbortCriteria
type RolloutState = types.RolloutState
type RolloutFilter = types.RolloutFilter
type APIToken = types.APIToken

// Re-export constants
const (
	AgentStatusOnline  = types.AgentStatusOnline
	AgentStatusOffline = types.AgentStatusOffline
	AgentStatusError   = types.AgentStatusError

	AlertSeverityInfo     = types.AlertSeverityInfo
	AlertSeverityWarning  = types.AlertSeverityWarning
	AlertSeverityCritical = types.AlertSeverityCritical

	ThresholdGreater        = types.ThresholdGreater
	ThresholdGreaterOrEqual = types.ThresholdGreaterOrEqual
	ThresholdLess           = types.ThresholdLess
	ThresholdLessOrEqual    = types.ThresholdLessOrEqual
	ThresholdEqual          = types.ThresholdEqual
	ThresholdNotEqual       = types.ThresholdNotEqual

	RolloutStatePending         = types.RolloutStatePending
	RolloutStateInProgress      = types.RolloutStateInProgress
	RolloutStatePaused          = types.RolloutStatePaused
	RolloutStateSucceeded       = types.RolloutStateSucceeded
	RolloutStateAborted         = types.RolloutStateAborted
	RolloutStateRolledBack      = types.RolloutStateRolledBack
	RolloutStatePendingApproval = types.RolloutStatePendingApproval
	RolloutStateRejected        = types.RolloutStateRejected

	RolloutStageModePercent = types.RolloutStageModePercent
	RolloutStageModeLabel   = types.RolloutStageModeLabel
)
