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
// to the broker so live timeline UIs can append in real time.
type AuditServiceImpl struct {
	appStore applicationstore.ApplicationStore
	broker   *events.Broker // optional
	logger   *zap.Logger
}

// NewAuditService creates an AuditService. The broker is optional — pass
// nil in tests to skip the SSE side effect.
func NewAuditService(appStore applicationstore.ApplicationStore, broker *events.Broker, logger *zap.Logger) AuditService {
	return &AuditServiceImpl{appStore: appStore, broker: broker, logger: logger}
}

// Record persists an entry. Stamps an id and timestamps if absent. Defensive
// against partial writes: failure to persist is logged at Warn and returned;
// failure to publish (broker full) is silent (the broker drops, not
// records) — the durable record wins.
func (s *AuditServiceImpl) Record(ctx context.Context, entry AuditEntry) error {
	now := time.Now().UTC()
	stored := &applicationstore.AuditEvent{
		ID:         uuid.New().String(),
		Timestamp:  now,
		Actor:      entry.Actor,
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
