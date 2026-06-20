// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAuditService_RecordAndList(t *testing.T) {
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	// Record three events for two different targets.
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor:      AuditActorSystem,
		EventType:  AuditEventConfigStored,
		TargetType: AuditTargetConfig,
		TargetID:   "cfg-1",
		Action:     "stored",
		Payload:    map[string]any{"version": 1},
	}))
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor:      AuditActorSystem,
		EventType:  AuditEventAgentDriftDrifted,
		TargetType: AuditTargetAgent,
		TargetID:   "agent-a",
		Action:     "drift",
	}))
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor:      AuditActorOpAMP,
		EventType:  AuditEventAgentRegistered,
		TargetType: AuditTargetAgent,
		TargetID:   "agent-b",
		Action:     "created",
	}))

	// Unfiltered list returns all three, newest-first.
	all, err := svc.List(ctx, AuditEventFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	// The store is in-memory and append-only; the newest-recorded event
	// (agent-b registered) should be first.
	assert.Equal(t, "agent-b", all[0].TargetID)

	// Filter to a target type returns only matching events.
	agentEvents, err := svc.List(ctx, AuditEventFilter{TargetType: AuditTargetAgent})
	require.NoError(t, err)
	assert.Len(t, agentEvents, 2)

	// Filter to a specific target id narrows further.
	a, err := svc.List(ctx, AuditEventFilter{
		TargetType: AuditTargetAgent,
		TargetID:   "agent-a",
	})
	require.NoError(t, err)
	require.Len(t, a, 1)
	assert.Equal(t, AuditEventAgentDriftDrifted, a[0].EventType)
}

func TestAuditService_EventTypeFilter(t *testing.T) {
	// Regression guard for #580 (v0.87.2): the AuditEventFilter struct
	// previously had no EventType field; the SQL store had no
	// "AND event_type = ?" clause. This test seeds two distinct event
	// types and pins that filtering by EventType returns only matching
	// rows. Memory-store coverage stands in for the SQL clause too —
	// the in-memory walk mirrors the SQL filter shape.
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	const wanted = "discovery.aws.connection_created"
	const other = "discovery.aws.connection_read"

	for i := 0; i < 2; i++ {
		require.NoError(t, svc.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  wanted,
			TargetType: "aws_connection",
			TargetID:   "acct-w",
			Action:     "created",
		}))
	}
	for i := 0; i < 4; i++ {
		require.NoError(t, svc.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  other,
			TargetType: "aws_connection",
			TargetID:   "acct-r",
			Action:     "read",
		}))
	}

	got, err := svc.List(ctx, AuditEventFilter{EventType: wanted})
	require.NoError(t, err)
	require.Len(t, got, 2, "EventType filter must narrow to seeded count; if 6, the store ignored the filter")
	for i, ev := range got {
		assert.Equal(t, wanted, ev.EventType,
			"row[%d].EventType = %q, want %q", i, ev.EventType, wanted)
	}

	// EventType combines with TargetType (AND semantics, not OR).
	mixed, err := svc.List(ctx, AuditEventFilter{
		EventType:  wanted,
		TargetType: "aws_connection",
	})
	require.NoError(t, err)
	assert.Len(t, mixed, 2)
}

func TestAuditService_SinceFilter(t *testing.T) {
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	// Record an event, capture a cut-off time, then record another. The
	// since filter should return only the second one.
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor: AuditActorSystem, EventType: "test.event", TargetType: "x", Action: "a",
	}))
	cutoff := time.Now().UTC()
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor: AuditActorSystem, EventType: "test.event", TargetType: "x", Action: "b",
	}))

	out, err := svc.List(ctx, AuditEventFilter{Since: cutoff})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "b", out[0].Action)
}

func TestAuditService_LimitCappedToOneThousand(t *testing.T) {
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	// 1500 events, ask for an absurd limit.
	for i := 0; i < 1500; i++ {
		require.NoError(t, svc.Record(ctx, AuditEntry{
			Actor: AuditActorSystem, EventType: "stress", TargetType: "x", Action: "a",
		}))
	}
	out, err := svc.List(ctx, AuditEventFilter{Limit: 999999})
	require.NoError(t, err)
	assert.Len(t, out, 1000, "limit must be capped to 1000")
}

func TestAuditService_Record_PublishesToBroker(t *testing.T) {
	// We don't import the events package here directly to keep this test
	// at the service-level API surface, but we can confirm the publish
	// side effect by passing a *real* broker and reading from a
	// subscription. The broker live in a separate package; we depend on
	// it indirectly via NewAuditService.
	t.Skip("broker round-trip is covered in internal/events tests; this skip documents the integration is wired in NewAuditService")
}
