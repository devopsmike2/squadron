package applicationstore

// Re-export types from the types package for convenience
import "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"

// Type aliases for convenience
type ApplicationStore = types.ApplicationStore

// v0.89.17 (#633) — re-export so the proposer bridge can name the
// method on the storage interface without dragging in the types
// package explicitly. The bridge already depends on
// applicationstore.Group; ApplicationStore is the surface that
// carries ListAIVerdictsForGroup.

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

// DiscoveryVerdict — v0.89.36 (#655 Stream 53). Renamed from
// AcceptedRecommendation (v0.89.28). Widened with State so the
// discovery bridge can project both pr_merged and
// pr_closed_not_merged audit-event rows through the shared
// verdictsel/verdictprompt pipeline.
type DiscoveryVerdict = types.DiscoveryVerdict

// ExcludedRecommendation — v0.89.37 (#656 Stream 54, #531 slice 2
// chunk 4). Projection over the new iac_recommendation_verdicts
// table for the discovery proposer's operator-set exclusion signal.
type ExcludedRecommendation = types.ExcludedRecommendation

// CheckRunRef — v0.89.42 (#662 Stream 60, slice 1 chunk 1 of the
// GitHub Checks API back-signal arc). Storage-layer mirror of the
// internal/iac/github CheckRunRef. Defined in the types package so
// the storage layer carries the projection without importing the
// iac/github client; the two types are field-compatible and the
// bridge converts between them at the boundary in chunk 2.
type CheckRunRef = types.CheckRunRef
type Rollout = types.Rollout
type RolloutStage = types.RolloutStage
type RolloutStageMode = types.RolloutStageMode
type RolloutAbortCriteria = types.RolloutAbortCriteria
type RolloutState = types.RolloutState
type RolloutFilter = types.RolloutFilter
type RolloutEvidenceRef = types.RolloutEvidenceRef
type APIToken = types.APIToken
type SiemDestination = types.SiemDestination

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

	// v0.89.14 (#630) — action runner steps in plans.
	StepKindRollout = types.StepKindRollout
	StepKindAction  = types.StepKindAction
)

// ErrRolloutVersionConflict re-exports the storage-layer optimistic-concurrency
// sentinel so callers that reference this package's aliased rollout types (the
// rollout engine, in particular) can match the conflict via errors.Is without
// importing the types package directly.
var ErrRolloutVersionConflict = types.ErrRolloutVersionConflict
