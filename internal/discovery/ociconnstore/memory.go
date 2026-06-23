// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ociconnstore

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
	byID    map[string]*OCIConnection // primary store, keyed by ID
	timeNow func() time.Time          // injectable so tests can pin timestamps
	newUUID func() string             // injectable so tests can pin IDs
}

// NewMemoryStore returns a fresh in-memory Store. Safe for concurrent
// use. Use this for tests, single-process dev runs, or air-gapped
// POCs that don't need persistence.
func NewMemoryStore() Store {
	return &memoryStore{
		byID:    make(map[string]*OCIConnection),
		timeNow: func() time.Time { return time.Now().UTC() },
		newUUID: func() string { return uuid.NewString() },
	}
}

// Create inserts a new connection. Mirrors sqliteStore.Create: same
// validation, same ID stamping.
func (m *memoryStore) Create(_ context.Context, conn *OCIConnection) error {
	if conn == nil {
		return errors.New("ociconnstore: Create: conn is required")
	}
	if conn.DisplayName == "" {
		return errors.New("ociconnstore: Create: DisplayName is required")
	}
	if conn.TenancyOCID == "" {
		return errors.New("ociconnstore: Create: TenancyOCID is required")
	}
	if conn.UserOCID == "" {
		return errors.New("ociconnstore: Create: UserOCID is required")
	}
	if conn.Fingerprint == "" {
		return errors.New("ociconnstore: Create: Fingerprint is required")
	}
	if len(conn.SealedPrivateKey) == 0 {
		return errors.New("ociconnstore: Create: SealedPrivateKey is required (callers must seal via credstore.SealOCIPrivateKey)")
	}
	if conn.Region == "" {
		return errors.New("ociconnstore: Create: Region is required (OCI's API endpoints are regional)")
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

	stored := cloneConnection(conn)
	m.byID[conn.ID] = stored
	return nil
}

// Get returns the connection for the supplied ID, or
// ErrConnectionNotFound if no row matches. Returns a defensive copy
// so callers can mutate the result without touching the store.
func (m *memoryStore) Get(_ context.Context, id string) (*OCIConnection, error) {
	if id == "" {
		return nil, errors.New("ociconnstore: Get: id is required")
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
func (m *memoryStore) List(_ context.Context) ([]*OCIConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*OCIConnection, 0, len(m.byID))
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
// conn.ID and stamps UpdatedAt. A nil/empty SealedPrivateKey leaves
// the stored sealed bytes in place; a fresh sealed blob rotates
// them. Returns ErrConnectionNotFound when no row matches.
func (m *memoryStore) Update(_ context.Context, conn *OCIConnection) error {
	if conn == nil {
		return errors.New("ociconnstore: Update: conn is required")
	}
	if conn.ID == "" {
		return errors.New("ociconnstore: Update: ID is required")
	}
	if conn.DisplayName == "" {
		return errors.New("ociconnstore: Update: DisplayName is required")
	}
	if conn.TenancyOCID == "" {
		return errors.New("ociconnstore: Update: TenancyOCID is required")
	}
	if conn.UserOCID == "" {
		return errors.New("ociconnstore: Update: UserOCID is required")
	}
	if conn.Fingerprint == "" {
		return errors.New("ociconnstore: Update: Fingerprint is required")
	}
	if conn.Region == "" {
		return errors.New("ociconnstore: Update: Region is required (OCI's API endpoints are regional)")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.byID[conn.ID]
	if !ok {
		return ErrConnectionNotFound
	}
	existing.DisplayName = conn.DisplayName
	existing.TenancyOCID = conn.TenancyOCID
	existing.UserOCID = conn.UserOCID
	existing.Fingerprint = conn.Fingerprint
	existing.Region = conn.Region
	existing.LearnFromAcceptedRecommendations = conn.LearnFromAcceptedRecommendations
	if len(conn.SealedPrivateKey) > 0 {
		// Defensive copy so a caller-side mutation post-call doesn't
		// alias the stored bytes.
		copied := make([]byte, len(conn.SealedPrivateKey))
		copy(copied, conn.SealedPrivateKey)
		existing.SealedPrivateKey = copied
	}
	existing.UpdatedAt = m.timeNow()
	conn.UpdatedAt = existing.UpdatedAt
	return nil
}

// Delete removes the row. Idempotent.
func (m *memoryStore) Delete(_ context.Context, id string) error {
	if id == "" {
		return errors.New("ociconnstore: Delete: id is required")
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
// aliasing. The SealedPrivateKey blob is copied so a caller mutating
// its input after Create / Get doesn't tamper with the stored row.
//
// Unlike iacconnstore.cloneConnection, the SealedPrivateKey field IS
// populated on the returned struct because the scanner needs the
// sealed bytes to call credstore.UnsealOCIPrivateKey. The defense-
// in-depth posture here lives on the JSON marshal path (json:"-" on
// the field) and in the audit/handler layer (chunk 3), not on the
// Store boundary.
func cloneConnection(in *OCIConnection) *OCIConnection {
	out := *in
	if in.SealedPrivateKey != nil {
		out.SealedPrivateKey = make([]byte, len(in.SealedPrivateKey))
		copy(out.SealedPrivateKey, in.SealedPrivateKey)
	}
	return &out
}
