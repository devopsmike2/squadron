// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// recordingAudit captures Record calls so the test can assert the
// api_token.revoked events (and their reason) emitted by RevokeByConnection.
type recordingAudit struct{ entries []AuditEntry }

func (r *recordingAudit) Record(_ context.Context, e AuditEntry) error {
	r.entries = append(r.entries, e)
	return nil
}
func (r *recordingAudit) List(context.Context, AuditEventFilter) ([]*AuditEvent, error) {
	return nil, nil
}
func (r *recordingAudit) Get(context.Context, string) (*AuditEvent, error) { return nil, nil }
func (r *recordingAudit) SetExplanation(context.Context, string, string, string, time.Time) error {
	return nil
}

func seedToken(t *testing.T, store applicationstore.ApplicationStore, id, connID string) {
	t.Helper()
	require.NoError(t, store.CreateAPIToken(context.Background(), &applicationstore.APIToken{
		ID:           id,
		Label:        "oidc:" + id,
		Hash:         "hash-" + id,
		Scopes:       []string{ScopeWildcard},
		CreatedAt:    time.Now().UTC(),
		ConnectionID: connID,
	}))
}

// TestRevokeByConnection revokes exactly the tokens minted through the deleted
// connection, leaves other connections' (and manual) tokens alone, and emits one
// api_token.revoked{reason:connection_deleted} audit event per revoked token.
func TestRevokeByConnection(t *testing.T) {
	store := memory.NewStore()
	seedToken(t, store, "a1", "conn-1")
	seedToken(t, store, "a2", "conn-1")
	seedToken(t, store, "b1", "conn-2")
	seedToken(t, store, "m1", "") // manual/bootstrap token, no connection

	audit := &recordingAudit{}
	svc := NewAuthService(store, zap.NewNop()).(*AuthServiceImpl)
	svc.SetAuditService(audit)

	n, err := svc.RevokeByConnection(context.Background(), "conn-1")
	require.NoError(t, err)
	require.Equal(t, 2, n, "both conn-1 tokens revoked")

	// conn-1 tokens revoked; conn-2 + manual untouched.
	all, err := store.ListAPITokens(context.Background())
	require.NoError(t, err)
	revoked := map[string]bool{}
	for _, tok := range all {
		revoked[tok.ID] = tok.RevokedAt != nil
	}
	require.True(t, revoked["a1"], "a1 should be revoked")
	require.True(t, revoked["a2"], "a2 should be revoked")
	require.False(t, revoked["b1"], "b1 (conn-2) must NOT be revoked")
	require.False(t, revoked["m1"], "manual token must NOT be revoked")

	// Exactly two audit events, both api_token.revoked with the connection reason.
	require.Len(t, audit.entries, 2)
	for _, e := range audit.entries {
		require.Equal(t, "api_token.revoked", e.EventType)
		require.Equal(t, "revoked", e.Action)
		require.Equal(t, "connection_deleted", e.Payload["reason"])
		require.Equal(t, "conn-1", e.Payload["connection_id"])
	}

	// Idempotent: a second call revokes nothing (already-revoked excluded).
	n2, err := svc.RevokeByConnection(context.Background(), "conn-1")
	require.NoError(t, err)
	require.Equal(t, 0, n2)

	// Empty connection id is a no-op (never matches the always-empty manual tokens).
	n3, err := svc.RevokeByConnection(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, 0, n3)
}

// VerifyChain — ADR 0027 slice 1. Test stub: self-tenant audit chain
// verify. Not exercised by these tests; returns a trivially OK result.
func (r *recordingAudit) VerifyChain(context.Context) (*applicationstore.AuditChainVerification, error) {
	return &applicationstore.AuditChainVerification{OK: true}, nil
}
