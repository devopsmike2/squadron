// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// connection_registry_test.go — HA S3b (ADR 0035) connection registry, Postgres
// backend. TEST_POSTGRES_DSN-gated (skips locally without a Postgres). Mirrors
// the sqlite acceptance suite so both backends stay in lock-step:
// (a) connect writes the row with the local instance_id; (b) clean disconnect
// removes it; (c) heartbeat advances last_heartbeat_at (connected_at preserved);
// (d) a stale row is reclaimable and a reconnect re-owns it (last-writer-wins).

func TestPostgres_ConnectionRegistry_UpsertGetDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agentID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := s.UpsertConnectionOwner(ctx, agentID, "instance-A", now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetConnectionOwner(ctx, agentID)
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.InstanceID != "instance-A" {
		t.Fatalf("instance_id = %q, want instance-A", got.InstanceID)
	}
	if got.AgentID != agentID {
		t.Fatalf("agent_id = %v, want %v", got.AgentID, agentID)
	}

	if err := s.DeleteConnectionOwner(ctx, agentID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = s.GetConnectionOwner(ctx, agentID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("row still present after delete: %+v", got)
	}
	// Missing-row delete is a clean no-op.
	if err := s.DeleteConnectionOwner(ctx, uuid.New()); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestPostgres_ConnectionRegistry_HeartbeatAdvances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agentID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	t1 := time.Now().UTC().Truncate(time.Millisecond)

	if err := s.UpsertConnectionOwner(ctx, agentID, "instance-A", t0); err != nil {
		t.Fatalf("upsert t0: %v", err)
	}
	if err := s.UpsertConnectionOwner(ctx, agentID, "instance-A", t1); err != nil {
		t.Fatalf("upsert t1 (heartbeat): %v", err)
	}
	got, err := s.GetConnectionOwner(ctx, agentID)
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if d := got.ConnectedAt.Sub(t0); d > time.Second || d < -time.Second {
		t.Fatalf("connected_at drifted from t0 by %v (want preserved)", d)
	}
	if d := got.LastHeartbeatAt.Sub(t1); d > time.Second || d < -time.Second {
		t.Fatalf("last_heartbeat_at not advanced to t1 (off by %v)", d)
	}
	if !got.LastHeartbeatAt.After(got.ConnectedAt) {
		t.Fatalf("heartbeat %v not after connect %v", got.LastHeartbeatAt, got.ConnectedAt)
	}
}

func TestPostgres_ConnectionRegistry_StaleReclaimAndReadopt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agentID := uuid.New()
	live := uuid.New()

	if err := s.UpsertConnectionOwner(ctx, live, "instance-live", time.Now().UTC()); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	if err := s.UpsertConnectionOwner(ctx, agentID, "instance-A", time.Now().UTC().Add(-5*time.Minute)); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}

	cutoff := time.Now().UTC().Add(-time.Minute)
	n, err := s.ReclaimStaleConnectionOwners(ctx, cutoff)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d rows, want 1", n)
	}
	if got, _ := s.GetConnectionOwner(ctx, agentID); got != nil {
		t.Fatalf("stale row not evicted: %+v", got)
	}
	if got, _ := s.GetConnectionOwner(ctx, live); got == nil {
		t.Fatalf("fresh row wrongly evicted")
	}
	// Re-run no-op.
	if n, err := s.ReclaimStaleConnectionOwners(ctx, cutoff); err != nil || n != 0 {
		t.Fatalf("reclaim re-run: n=%d err=%v (want 0,nil)", n, err)
	}

	// Reconnect on instance-B re-owns (last-writer-wins), resetting connected_at.
	readopt := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.UpsertConnectionOwner(ctx, agentID, "instance-B", readopt); err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	got, err := s.GetConnectionOwner(ctx, agentID)
	if err != nil || got == nil {
		t.Fatalf("get re-adopted: err=%v got=%v", err, got)
	}
	if got.InstanceID != "instance-B" {
		t.Fatalf("re-adopt instance_id = %q, want instance-B", got.InstanceID)
	}
	if d := got.ConnectedAt.Sub(readopt); d > time.Second || d < -time.Second {
		t.Fatalf("takeover did not reset connected_at (off by %v)", d)
	}
}

func TestPostgres_ConnectionRegistry_List(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.UpsertConnectionOwner(ctx, uuid.New(), "instance-A", now); err != nil {
		t.Fatalf("upsert a1: %v", err)
	}
	if err := s.UpsertConnectionOwner(ctx, uuid.New(), "instance-B", now); err != nil {
		t.Fatalf("upsert a2: %v", err)
	}
	owners, err := s.ListConnectionOwners(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(owners) != 2 {
		t.Fatalf("list returned %d owners, want 2", len(owners))
	}
}
