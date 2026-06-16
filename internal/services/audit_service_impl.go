// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// AuditServiceImpl is the canonical AuditService implementation backed by
// the application store. It also publishes a `audit_event_recorded` event
// to the broker so live timeline UIs can append in real time, and (if
// configured) fans the event out to a SelfTelemetryPublisher so the
// entry shows up in the operator's external observability stack as an
// OTel span.
type AuditServiceImpl struct {
	appStore  applicationstore.ApplicationStore
	broker    *events.Broker             // optional
	selftel   SelfTelemetryPublisher     // optional
	siem      SiemDispatcher             // optional, v0.50
	logger    *zap.Logger
}

// SiemDispatcher is the slim contract AuditServiceImpl uses to fan
// audit events out to external SIEM destinations. Defined here (not
// as a direct reference to siem.Dispatcher) so services/ doesn't
// take a build-time dependency on internal/siem — the real
// dispatcher satisfies this interface. Nil-safe: a nil dispatcher
// means SIEM export is disabled.
//
// Added in v0.50 for centralized audit retention at regulated
// environments (NERC CIP, SOX, etc.) that need 3-7 year audit
// retention outside Squadron's local DB.
type SiemDispatcher interface {
	Dispatch(ev SiemEvent)
}

// SiemEvent is the contract shape SiemDispatcher receives. Mirrors
// the on-the-wire shape (siem.Event) without taking a build-time
// dependency on the siem package.
type SiemEvent struct {
	ID         string
	Timestamp  time.Time
	Actor      string
	EventType  string
	TargetType string
	TargetID   string
	Action     string
	Payload    map[string]any
}

// SetSiemDispatcher swaps the SIEM fan-out target post-construction.
// Used so main.go can build the dispatcher after the audit service
// (audit service is wired earlier in the dependency graph).
func (s *AuditServiceImpl) SetSiemDispatcher(d SiemDispatcher) {
	s.siem = d
}

// SelfTelemetryPublisher is the slim contract AuditServiceImpl needs to
// fan out audit events as OTel spans. Defined here (not as a direct
// reference to selftel.Publisher) so services/ doesn't import selftel/
// — the audit service is below selftel in the dependency graph and the
// real publisher just satisfies this interface.
type SelfTelemetryPublisher interface {
	PublishAuditEvent(ctx context.Context, entry SelfTelemetryEntry)
}

// SelfTelemetryEntry is the contract shape SelfTelemetryPublisher
// receives. Mirrors the selftel package's AuditEntry without taking a
// build-time dependency on it.
type SelfTelemetryEntry struct {
	Actor      string
	EventType  string
	TargetType string
	TargetID   string
	Action     string
	Payload    map[string]any
}

// NewAuditService creates an AuditService. The broker is optional —
// pass nil in tests to skip the SSE side effect. selftel is optional —
// pass nil when self-telemetry is disabled (the common case for dev
// instances).
func NewAuditService(
	appStore applicationstore.ApplicationStore,
	broker *events.Broker,
	logger *zap.Logger,
) AuditService {
	return &AuditServiceImpl{appStore: appStore, broker: broker, logger: logger}
}

// NewAuditServiceWithSelfTelemetry is the production constructor used
// when telemetry.enabled is true. Identical to NewAuditService except
// for the OTel publisher wiring. Keeping it a separate constructor
// avoids adding a nil parameter to every NewAuditService caller in
// existing tests.
func NewAuditServiceWithSelfTelemetry(
	appStore applicationstore.ApplicationStore,
	broker *events.Broker,
	selftel SelfTelemetryPublisher,
	logger *zap.Logger,
) AuditService {
	return &AuditServiceImpl{
		appStore: appStore,
		broker:   broker,
		selftel:  selftel,
		logger:   logger,
	}
}

// Record persists an entry. Stamps an id and timestamps if absent. Defensive
// against partial writes: failure to persist is logged at Warn and returned;
// failure to publish (broker full) is silent (the broker drops, not
// records) — the durable record wins.
//
// If the context carries an AuthActor (set by the bearer middleware),
// that actor overrides whatever the caller put in entry.Actor. This
// way authenticated requests get attributed to the issuing token
// without every handler having to plumb the actor through manually.
// Background operations (the rollout engine, the alert evaluator) call
// Record with a plain context.Background() and keep their original
// "system" actor unchanged.
func (s *AuditServiceImpl) Record(ctx context.Context, entry AuditEntry) error {
	actor := entry.Actor
	if a := ActorFromContext(ctx); !a.IsZero() {
		actor = a.String()
	}
	now := time.Now().UTC()
	stored := &applicationstore.AuditEvent{
		ID:         uuid.New().String(),
		Timestamp:  now,
		Actor:      actor,
		EventType:  entry.EventType,
		TargetType: entry.TargetType,
		TargetID:   entry.TargetID,
		Action:     entry.Action,
		Payload:    entry.Payload,
		CreatedAt:  now,
	}
	if err := s.appStore.CreateAuditEvent(ctx, stored); err != nil {
		s.logger.Warn("failed to record audit event",
			zap.String("event_type", entry.EventType),
			zap.String("target_type", entry.TargetType),
			zap.String("target_id", entry.TargetID),
			zap.Error(err))
		return err
	}

	if s.broker != nil {
		s.broker.Publish(events.Event{
			Type: "audit_event_recorded",
			At:   now,
			Data: map[string]any{
				"id":          stored.ID,
				"event_type":  stored.EventType,
				"target_type": stored.TargetType,
				"target_id":   stored.TargetID,
				"action":      stored.Action,
				"actor":       stored.Actor,
			},
		})
	}
	// Self-telemetry fan-out. Best-effort: the OTel SDK's batch
	// processor handles network failures internally and we don't
	// surface them up — the durable SQLite row is the source of
	// truth, OTel export is a convenience for external observability.
	if s.selftel != nil {
		s.selftel.PublishAuditEvent(ctx, SelfTelemetryEntry{
			Actor:      actor,
			EventType:  entry.EventType,
			TargetType: entry.TargetType,
			TargetID:   entry.TargetID,
			Action:     entry.Action,
			Payload:    entry.Payload,
		})
	}
	// v0.50 — SIEM fan-out. Best-effort, non-blocking: the
	// dispatcher's bounded queues drop on overflow rather than
	// stalling the audit write path. Local SQLite is the source
	// of truth; SIEM is a convenience for compliance retention.
	if s.siem != nil {
		s.siem.Dispatch(SiemEvent{
			ID:         stored.ID,
			Timestamp:  now,
			Actor:      actor,
			EventType:  entry.EventType,
			TargetType: entry.TargetType,
			TargetID:   entry.TargetID,
			Action:     entry.Action,
			Payload:    entry.Payload,
		})
	}
	return nil
}

func (s *AuditServiceImpl) List(ctx context.Context, filter AuditEventFilter) ([]*AuditEvent, error) {
	stored, err := s.appStore.ListAuditEvents(ctx, applicationstore.AuditEventFilter{
		TargetType: filter.TargetType,
		TargetID:   filter.TargetID,
		Since:      filter.Since,
		Limit:      filter.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*AuditEvent, len(stored))
	for i, e := range stored {
		out[i] = &AuditEvent{
			ID:         e.ID,
			Timestamp:  e.Timestamp,
			Actor:      e.Actor,
			EventType:  e.EventType,
			TargetType: e.TargetType,
			TargetID:   e.TargetID,
			Action:     e.Action,
			Payload:    e.Payload,
			CreatedAt:  e.CreatedAt,
		}
	}
	return out, nil
}
