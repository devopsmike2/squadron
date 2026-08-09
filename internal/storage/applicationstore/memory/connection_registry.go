// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
)

// connection_registry.go — the in-memory mirror of the HA S3b connection
// registry (ADR 0035). Semantics mirror the sqlite/postgres backends EXACTLY.
// SYSTEM-SCOPED: no tenant dimension; the context's tenant is never read.

// UpsertConnectionOwner records (or refreshes) ownership of agentID. Fresh row
// → connected_at = now; heartbeat from the SAME instance → connected_at
// preserved, only last_heartbeat_at advances; takeover by a DIFFERENT instance
// → connected_at reset to now.
func (s *Store) UpsertConnectionOwner(_ context.Context, agentID uuid.UUID, instanceID string, now time.Time) error {
	if instanceID == "" {
		return fmt.Errorf("instance_id required")
	}
	ts := now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	connectedAt := ts
	if existing, ok := s.connectionOwners[agentID]; ok && existing.InstanceID == instanceID {
		// Heartbeat from the same owner: preserve the original connect time.
		connectedAt = existing.ConnectedAt
	}
	s.connectionOwners[agentID] = types.ConnectionOwner{
		AgentID:         agentID,
		InstanceID:      instanceID,
		ConnectedAt:     connectedAt,
		LastHeartbeatAt: ts,
	}
	return nil
}

// DeleteConnectionOwner removes agentID's ownership row. Missing row → no-op.
func (s *Store) DeleteConnectionOwner(_ context.Context, agentID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.connectionOwners, agentID)
	return nil
}

// GetConnectionOwner returns the ownership row for agentID, or (nil, nil) when
// no row exists.
func (s *Store) GetConnectionOwner(_ context.Context, agentID uuid.UUID) (*types.ConnectionOwner, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, ok := s.connectionOwners[agentID]
	if !ok {
		return nil, nil
	}
	cp := owner
	return &cp, nil
}

// ListConnectionOwners returns every ownership row ordered by agent_id.
func (s *Store) ListConnectionOwners(_ context.Context) ([]*types.ConnectionOwner, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owners := make([]*types.ConnectionOwner, 0, len(s.connectionOwners))
	for _, owner := range s.connectionOwners {
		cp := owner
		owners = append(owners, &cp)
	}
	sort.Slice(owners, func(i, j int) bool {
		return owners[i].AgentID.String() < owners[j].AgentID.String()
	})
	return owners, nil
}

// ReclaimStaleConnectionOwners deletes rows whose last_heartbeat_at is strictly
// older than olderThan and returns the count removed.
func (s *Store) ReclaimStaleConnectionOwners(_ context.Context, olderThan time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for id, owner := range s.connectionOwners {
		if owner.LastHeartbeatAt.Before(olderThan) {
			delete(s.connectionOwners, id)
			deleted++
		}
	}
	return deleted, nil
}
