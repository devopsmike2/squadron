// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// memoryStore is the in-memory Store implementation used by tests and
// any deployment that wants ephemeral state. Matches the SQLite
// implementation's contract one-for-one: same validation, same error
// sentinels, same UPSERT-vs-conflict semantics. Backed by a
// connection-id-keyed map plus a (provider, repo_full_name) lookup
// index for the uniqueness guard.
type memoryStore struct {
	mu      sync.RWMutex
	byID    map[string]*IaCConnection // primary store, keyed by ConnectionID
	timeNow func() time.Time          // injectable so tests can pin timestamps
	newUUID func() string             // injectable so tests can pin IDs
}

// NewMemoryStore returns a fresh in-memory Store. Safe for concurrent
// use. Use this for tests, single-process dev runs, or air-gapped
// POCs that don't need persistence.
func NewMemoryStore() Store {
	return &memoryStore{
		byID:    make(map[string]*IaCConnection),
		timeNow: func() time.Time { return time.Now().UTC() },
		newUUID: func() string { return uuid.NewString() },
	}
}

// Create inserts a new connection. Mirrors sqliteStore.Create: same
// validation, same ConnectionID stamping, same ErrConnectionConflict
// signal when (Provider, RepoFullName) already exists.
func (m *memoryStore) Create(_ context.Context, conn *IaCConnection) error {
	if conn == nil {
		return errors.New("iacconnstore: Create: conn is required")
	}
	if conn.Provider == "" {
		return errors.New("iacconnstore: Create: Provider is required")
	}
	if conn.AuthKind == "" {
		return errors.New("iacconnstore: Create: AuthKind is required")
	}
	if conn.RepoFullName == "" {
		return errors.New("iacconnstore: Create: RepoFullName is required")
	}
	if conn.DefaultBranch == "" {
		return errors.New("iacconnstore: Create: DefaultBranch is required")
	}
	if conn.RepoLayout == "" {
		return errors.New("iacconnstore: Create: RepoLayout is required")
	}
	if len(conn.CredCiphertext) == 0 {
		return errors.New("iacconnstore: Create: CredCiphertext is required (callers must seal via MarshalGitHubPATCreds)")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, existing := range m.byID {
		if existing.Provider == conn.Provider && existing.RepoFullName == conn.RepoFullName {
			return fmt.Errorf("%w: provider=%s repo=%s", ErrConnectionConflict, conn.Provider, conn.RepoFullName)
		}
	}

	now := m.timeNow()
	conn.ConnectionID = m.newUUID()
	conn.CreatedAt = now
	conn.UpdatedAt = now
	// Default LearnFromAcceptedRecommendations=true at Create time.
	// v0.89.28 (#643 slice 1): the design's per-connection opt-in flag
	// defaults on so the discovery feedback loop is active for every
	// fresh connection. The PATCH endpoint is the documented opt-out.
	if !conn.LearnFromAcceptedRecommendations {
		conn.LearnFromAcceptedRecommendations = true
	}

	// Defensive copy so a caller mutating its struct after Create
	// doesn't mutate the stored row.
	stored := cloneConnection(conn)
	m.byID[conn.ConnectionID] = stored
	return nil
}

// Get returns the connection for the supplied ID, or
// ErrConnectionNotFound if no row matches. Returns a defensive copy
// so callers can mutate the result without touching the store.
func (m *memoryStore) Get(_ context.Context, connectionID string) (*IaCConnection, error) {
	if connectionID == "" {
		return nil, errors.New("iacconnstore: Get: connectionID is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	conn, ok := m.byID[connectionID]
	if !ok {
		return nil, ErrConnectionNotFound
	}
	return cloneConnection(conn), nil
}

// GetByRepoFullName scans the map for connections matching repoFullName
// and returns the most recently created one. Mirrors the SQLite
// implementation's ORDER BY created_at DESC LIMIT 1 contract.
func (m *memoryStore) GetByRepoFullName(_ context.Context, repoFullName string) (*IaCConnection, error) {
	if repoFullName == "" {
		return nil, errors.New("iacconnstore: GetByRepoFullName: repoFullName is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var newest *IaCConnection
	for _, existing := range m.byID {
		if existing.RepoFullName != repoFullName {
			continue
		}
		if newest == nil || existing.CreatedAt.After(newest.CreatedAt) {
			newest = existing
		}
	}
	if newest == nil {
		return nil, ErrConnectionNotFound
	}
	return cloneConnection(newest), nil
}

// List returns every connection, ordered by created_at ascending then
// connection_id ascending (same order as the SQLite implementation
// for deterministic tests).
func (m *memoryStore) List(_ context.Context) ([]*IaCConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*IaCConnection, 0, len(m.byID))
	for _, conn := range m.byID {
		out = append(out, cloneConnection(conn))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ConnectionID < out[j].ConnectionID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the row. Idempotent.
func (m *memoryStore) Delete(_ context.Context, connectionID string) error {
	if connectionID == "" {
		return errors.New("iacconnstore: Delete: connectionID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, connectionID)
	return nil
}

// UpdatePlacementMap replaces the PlacementMap and stamps UpdatedAt.
func (m *memoryStore) UpdatePlacementMap(_ context.Context, connectionID string, entries []PlacementMapEntry) error {
	if connectionID == "" {
		return errors.New("iacconnstore: UpdatePlacementMap: connectionID is required")
	}
	if entries == nil {
		entries = []PlacementMapEntry{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	conn, ok := m.byID[connectionID]
	if !ok {
		return ErrConnectionNotFound
	}
	// Copy the slice so a caller-side mutation post-call doesn't
	// alias the stored entries.
	copied := make([]PlacementMapEntry, len(entries))
	copy(copied, entries)
	conn.PlacementMap = copied
	conn.UpdatedAt = m.timeNow()
	return nil
}

// UpdateLearnFromAcceptedRecommendations sets the per-connection
// discovery-feedback opt-in flag and stamps UpdatedAt.
// v0.89.28 (#643 slice 1).
func (m *memoryStore) UpdateLearnFromAcceptedRecommendations(_ context.Context, connectionID string, learn bool) error {
	if connectionID == "" {
		return errors.New("iacconnstore: UpdateLearnFromAcceptedRecommendations: connectionID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	conn, ok := m.byID[connectionID]
	if !ok {
		return ErrConnectionNotFound
	}
	conn.LearnFromAcceptedRecommendations = learn
	conn.UpdatedAt = m.timeNow()
	return nil
}

// Close is a no-op for the memory store. The interface contract is
// satisfied; subsequent calls remain valid.
func (m *memoryStore) Close() error {
	return nil
}

// cloneConnection returns a deep-enough copy that callers and the
// store cannot share mutable state through slice or byte-slice
// aliasing. Maps and pointers are not used in IaCConnection, so the
// slice copies are sufficient.
func cloneConnection(in *IaCConnection) *IaCConnection {
	out := *in
	if in.CredCiphertext != nil {
		out.CredCiphertext = make([]byte, len(in.CredCiphertext))
		copy(out.CredCiphertext, in.CredCiphertext)
	}
	if in.PlacementMap != nil {
		out.PlacementMap = make([]PlacementMapEntry, len(in.PlacementMap))
		copy(out.PlacementMap, in.PlacementMap)
	}
	return &out
}
