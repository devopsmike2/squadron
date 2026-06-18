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
}

// AuditEventFilter narrows a List query. All fields are optional.
type AuditEventFilter struct {
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
	AuditEventAgentRegistered    = "agent.registered"
	AuditEventAgentDriftSynced   = "agent.drift.synced"
	AuditEventAgentDriftDrifted  = "agent.drift.drifted"
	AuditEventConfigStored       = "config.stored"
	AuditEventConfigApplied      = "config.applied"
	AuditEventAlertRuleCreated   = "alert_rule.created"
	AuditEventAlertRuleUpdated   = "alert_rule.updated"
	AuditEventAlertRuleDeleted   = "alert_rule.deleted"
	AuditEventAlertFired         = "alert.fired"
	AuditEventAlertResolved      = "alert.resolved"

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
	AuditEventIncidentDrafted        = "incident.drafted"
	AuditEventIncidentDraftDeclined  = "incident.draft_declined"
)
