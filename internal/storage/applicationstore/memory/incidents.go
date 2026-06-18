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

// CreateIncidentDraft persists a new draft. Deep copies the supplied
// pointer so the caller can mutate without affecting stored state.
// Returns an error if the ID already exists; the bridge avoids that
// by calling GetIncidentDraftByActionRequestID first.
func (s *Store) CreateIncidentDraft(_ context.Context, d *types.IncidentDraft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.incidentDrafts[d.ID]; exists {
		return fmt.Errorf("incident draft already exists: %s", d.ID)
	}
	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = now
	}
	if d.Status == "" {
		d.Status = "draft"
	}
	cp := *d
	s.incidentDrafts[d.ID] = &cp
	return nil
}

// UpdateIncidentDraft overwrites the mutable fields and bumps
// updated_at.
func (s *Store) UpdateIncidentDraft(_ context.Context, d *types.IncidentDraft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.incidentDrafts[d.ID]
	if !ok {
		return fmt.Errorf("incident draft not found: %s", d.ID)
	}
	stored.Status = d.Status
	stored.Title = d.Title
	stored.BodyMarkdown = d.BodyMarkdown
	stored.DraftContentJSON = d.DraftContentJSON
	stored.Provider = d.Provider
	stored.ExternalID = d.ExternalID
	stored.ExternalURL = d.ExternalURL
	stored.UpdatedAt = time.Now().UTC()
	return nil
}

// GetIncidentDraft returns the draft or nil when missing.
func (s *Store) GetIncidentDraft(_ context.Context, id string) (*types.IncidentDraft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.incidentDrafts[id]
	if !ok {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

// GetIncidentDraftByActionRequestID looks up a draft by the action
// request that triggered it. This is the bridge's dedup primary
// access path: before drafting a new ticket, check whether one
// already exists for this action.
func (s *Store) GetIncidentDraftByActionRequestID(_ context.Context, actionRequestID string) (*types.IncidentDraft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if actionRequestID == "" {
		return nil, nil
	}
	for _, d := range s.incidentDrafts {
		if d.ActionRequestID == actionRequestID {
			cp := *d
			return &cp, nil
		}
	}
	return nil, nil
}

// ListIncidentDrafts returns drafts that match the filter, newest
// created_at first.
func (s *Store) ListIncidentDrafts(_ context.Context, filter types.IncidentDraftFilter) ([]*types.IncidentDraft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var out []*types.IncidentDraft
	for _, d := range s.incidentDrafts {
		if filter.ActionRequestID != "" && d.ActionRequestID != filter.ActionRequestID {
			continue
		}
		if filter.RolloutID != "" && d.RolloutID != filter.RolloutID {
			continue
		}
		if filter.Status != "" && d.Status != filter.Status {
			continue
		}
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
