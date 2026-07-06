// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListAuditEvents_ActorAndTimeWindow pins the ADR 0020 foundation filters:
// exact-match Actor, and the Since (>=) / Until (<) time window that backs
// newest→oldest cursor pagination. Until is an EXCLUSIVE upper bound.
func TestListAuditEvents_ActorAndTimeWindow(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	seed := func(id, actor string, ts time.Time) {
		require.NoError(t, s.CreateAuditEvent(ctx, &types.AuditEvent{
			ID: id, Timestamp: ts, Actor: actor, EventType: "x.y", TargetType: "agent", Action: "z",
		}))
	}
	seed("e0", "operator:alice", base)                  // 12:00
	seed("e1", "operator:alice", base.Add(1*time.Hour)) // 13:00
	seed("e2", "operator:bob", base.Add(2*time.Hour))   // 14:00
	seed("e3", "operator:alice", base.Add(3*time.Hour)) // 15:00

	// Actor filter: only alice's three rows, newest-first.
	got, err := s.ListAuditEvents(ctx, types.AuditEventFilter{Actor: "operator:alice"})
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "e3", got[0].ID, "newest-first")

	// Half-open window [13:00, 15:00): includes 13:00 and 14:00, excludes 15:00.
	got, err = s.ListAuditEvents(ctx, types.AuditEventFilter{
		Since: base.Add(1 * time.Hour),
		Until: base.Add(3 * time.Hour),
	})
	require.NoError(t, err)
	ids := []string{}
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	assert.ElementsMatch(t, []string{"e1", "e2"}, ids, "Until is exclusive: 15:00 (e3) excluded, Since inclusive: 13:00 (e1) included")

	// Cursor step: everything strictly before 14:00, newest-first → e1 then e0.
	got, err = s.ListAuditEvents(ctx, types.AuditEventFilter{Until: base.Add(2 * time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "e1", got[0].ID)
	assert.Equal(t, "e0", got[1].ID)
}
