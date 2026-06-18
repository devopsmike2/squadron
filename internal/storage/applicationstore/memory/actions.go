// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// CreateActionRunnerRegistration stores the registration in memory.
// Deep-copies the supplied pointer so the caller can mutate the
// argument afterward without affecting stored state.
func (s *Store) CreateActionRunnerRegistration(_ context.Context, r *types.ActionRunnerRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.actionRunners[r.RunnerID]; exists {
		return fmt.Errorf("action runner registration already exists: %s", r.RunnerID)
	}
	if r.RegisteredAt.IsZero() {
		r.RegisteredAt = time.Now().UTC()
	}
	if r.LastSeenAt.IsZero() {
		r.LastSeenAt = r.RegisteredAt
	}
	cp := *r
	s.actionRunners[r.RunnerID] = &cp
	return nil
}

// UpdateActionRunnerRegistration overwrites every field except the
// primary key.
func (s *Store) UpdateActionRunnerRegistration(_ context.Context, r *types.ActionRunnerRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.actionRunners[r.RunnerID]; !ok {
		return fmt.Errorf("action runner registration not found: %s", r.RunnerID)
	}
	cp := *r
	s.actionRunners[r.RunnerID] = &cp
	return nil
}

// GetActionRunnerRegistration returns the registration or nil when
// missing.
func (s *Store) GetActionRunnerRegistration(_ context.Context, runnerID string) (*types.ActionRunnerRegistration, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.actionRunners[runnerID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// ListActionRunnerRegistrations returns every registration sorted
// newest registered_at first to match the SQLite implementation.
func (s *Store) ListActionRunnerRegistrations(_ context.Context) ([]*types.ActionRunnerRegistration, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.ActionRunnerRegistration, 0, len(s.actionRunners))
	for _, r := range s.actionRunners {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RegisteredAt.After(out[j].RegisteredAt)
	})
	return out, nil
}

// RevokeActionRunnerRegistration marks the registration's revoked_at
// timestamp.
func (s *Store) RevokeActionRunnerRegistration(_ context.Context, runnerID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.actionRunners[runnerID]
	if !ok {
		return fmt.Errorf("action runner registration not found: %s", runnerID)
	}
	t := at
	r.RevokedAt = &t
	return nil
}

// CreateActionRequest stores the request in memory.
func (s *Store) CreateActionRequest(_ context.Context, r *types.ActionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.actionRequests[r.ID]; exists {
		return fmt.Errorf("action request already exists: %s", r.ID)
	}
	if r.Status == "" {
		r.Status = "pending"
	}
	cp := *r
	s.actionRequests[r.ID] = &cp
	return nil
}

// UpdateActionRequest overwrites the mutable fields (status, output,
// timing) and leaves the immutable fields alone.
func (s *Store) UpdateActionRequest(_ context.Context, r *types.ActionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.actionRequests[r.ID]
	if !ok {
		return fmt.Errorf("action request not found: %s", r.ID)
	}
	stored.Status = r.Status
	stored.DeniedFor = r.DeniedFor
	stored.DryRunOutputJSON = r.DryRunOutputJSON
	stored.ExecutionOutputJSON = r.ExecutionOutputJSON
	stored.StartedAt = r.StartedAt
	stored.CompletedAt = r.CompletedAt
	return nil
}

// GetActionRequest returns the request or nil when missing.
func (s *Store) GetActionRequest(_ context.Context, id string) (*types.ActionRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.actionRequests[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// ListActionRequests returns matching requests, newest issued_at
// first.
func (s *Store) ListActionRequests(_ context.Context, filter types.ActionRequestFilter) ([]*types.ActionRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var out []*types.ActionRequest
	for _, r := range s.actionRequests {
		if filter.ProposalID != "" && r.ProposalID != filter.ProposalID {
			continue
		}
		if filter.RunnerID != "" && r.RunnerID != filter.RunnerID {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].IssuedAt.After(out[j].IssuedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
