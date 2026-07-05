// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// memoryStore is the in-memory Store implementation used by tests and
// any deployment that wants ephemeral state. Matches the SQLite
// implementation's contract one-for-one: same validation, same error
// sentinels, same UUID stamping, same defensive-copy semantics on
// the sealed bytes.
type memoryStore struct {
	mu      sync.RWMutex
	byID    map[string]*GCPConnection // primary store, keyed by ID
	timeNow func() time.Time          // injectable so tests can pin timestamps
	newUUID func() string             // injectable so tests can pin IDs
}

// NewMemoryStore returns a fresh in-memory Store. Safe for concurrent
// use. Use this for tests, single-process dev runs, or air-gapped
// POCs that don't need persistence.
func NewMemoryStore() Store {
	return &memoryStore{
		byID:    make(map[string]*GCPConnection),
		timeNow: func() time.Time { return time.Now().UTC() },
		newUUID: func() string { return uuid.NewString() },
	}
}

// Create inserts a new connection. Mirrors sqliteStore.Create: same
// validation, same ID stamping.
func (m *memoryStore) Create(_ context.Context, conn *GCPConnection) error {
	if conn == nil {
		return errors.New("gcpconnstore: Create: conn is required")
	}
	if conn.DisplayName == "" {
		return errors.New("gcpconnstore: Create: DisplayName is required")
	}
	if conn.ProjectID == "" {
		return errors.New("gcpconnstore: Create: ProjectID is required")
	}
	if len(conn.SealedSA) == 0 {
		return errors.New("gcpconnstore: Create: SealedSA is required (callers must seal via credstore.SealGCPServiceAccount)")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.timeNow()
	conn.ID = m.newUUID()
	conn.CreatedAt = now
	conn.UpdatedAt = now
	if !conn.LearnFromAcceptedRecommendations {
		conn.LearnFromAcceptedRecommendations = true
	}
	// ADR 0013 §D6-b: mirror sqliteStore.Create's owner-tenant
	// default-guard so a struct built without a tenant lands "default"
	// rather than an empty string.
	if conn.TenantID == "" {
		conn.TenantID = "default"
	}

	stored := cloneConnection(conn)
	m.byID[conn.ID] = stored
	return nil
}

// Get returns the connection for the supplied ID, or
// ErrConnectionNotFound if no row matches. Returns a defensive copy
// so callers can mutate the result without touching the store.
func (m *memoryStore) Get(_ context.Context, id string) (*GCPConnection, error) {
	if id == "" {
		return nil, errors.New("gcpconnstore: Get: id is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	conn, ok := m.byID[id]
	if !ok {
		return nil, ErrConnectionNotFound
	}
	return cloneConnection(conn), nil
}

// List returns every connection, ordered by created_at ascending then
// ID ascending (same order as the SQLite implementation for
// deterministic tests).
func (m *memoryStore) List(_ context.Context) ([]*GCPConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*GCPConnection, 0, len(m.byID))
	for _, conn := range m.byID {
		out = append(out, cloneConnection(conn))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Update replaces the mutable fields on the row identified by
// conn.ID and stamps UpdatedAt. A nil/empty SealedSA leaves the
// stored sealed bytes in place; a fresh sealed blob rotates them.
// Returns ErrConnectionNotFound when no row matches.
func (m *memoryStore) Update(_ context.Context, conn *GCPConnection) error {
	if conn == nil {
		return errors.New("gcpconnstore: Update: conn is required")
	}
	if conn.ID == "" {
		return errors.New("gcpconnstore: Update: ID is required")
	}
	if conn.DisplayName == "" {
		return errors.New("gcpconnstore: Update: DisplayName is required")
	}
	if conn.ProjectID == "" {
		return errors.New("gcpconnstore: Update: ProjectID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.byID[conn.ID]
	if !ok {
		return ErrConnectionNotFound
	}
	existing.DisplayName = conn.DisplayName
	existing.ProjectID = conn.ProjectID
	existing.Region = conn.Region
	existing.LearnFromAcceptedRecommendations = conn.LearnFromAcceptedRecommendations
	if len(conn.SealedSA) > 0 {
		// Defensive copy so a caller-side mutation post-call doesn't
		// alias the stored bytes.
		copied := make([]byte, len(conn.SealedSA))
		copy(copied, conn.SealedSA)
		existing.SealedSA = copied
	}
	existing.UpdatedAt = m.timeNow()
	conn.UpdatedAt = existing.UpdatedAt
	return nil
}

// Delete removes the row. Idempotent.
func (m *memoryStore) Delete(_ context.Context, id string) error {
	if id == "" {
		return errors.New("gcpconnstore: Delete: id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, id)
	return nil
}

// Close is a no-op for the memory store. The interface contract is
// satisfied; subsequent calls remain valid.
func (m *memoryStore) Close() error {
	return nil
}

// cloneConnection returns a deep-enough copy that callers and the
// store cannot share mutable state through slice or byte-slice
// aliasing. The SealedSA blob is copied so a caller mutating its
// input after Create / Get doesn't tamper with the stored row.
//
// Unlike iacconnstore.cloneConnection, the SealedSA field IS
// populated on the returned struct because the scanner needs the
// sealed bytes to call credstore.UnsealGCPServiceAccount. The
// defense-in-depth posture here lives on the JSON marshal path
// (json:"-" on the field) and in the audit/handler layer (chunk 3),
// not on the Store boundary.
func cloneConnection(in *GCPConnection) *GCPConnection {
	out := *in
	if in.SealedSA != nil {
		out.SealedSA = make([]byte, len(in.SealedSA))
		copy(out.SealedSA, in.SealedSA)
	}
	return &out
}
