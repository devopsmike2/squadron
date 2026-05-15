// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

func TestAuthService_Issue_ReturnsPlaintextOnce(t *testing.T) {
	// Plaintext must be returned exactly once at Issue. Subsequent
	// List/Validate calls don't return it. This is the core security
	// property of the cookbook.
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()

	token, plaintext, err := svc.Issue(ctx, "ci-bot")
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)
	require.True(t, strings.HasPrefix(plaintext, "sqd_"), "token should have human-readable prefix")
	require.NotEmpty(t, token.ID)
	require.Equal(t, "ci-bot", token.Label)
	require.NotZero(t, token.CreatedAt)
	require.Nil(t, token.RevokedAt)
	require.Nil(t, token.LastUsedAt)
}

func TestAuthService_Issue_LabelRequired(t *testing.T) {
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	_, _, err := svc.Issue(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "label")

	_, _, err = svc.Issue(context.Background(), "   ")
	require.Error(t, err, "whitespace-only labels should be rejected")
}

func TestAuthService_Issue_LabelLength(t *testing.T) {
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	long := strings.Repeat("x", labelMaxLen+1)
	_, _, err := svc.Issue(context.Background(), long)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chars or fewer")
}

func TestAuthService_Validate_RoundTrip(t *testing.T) {
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()

	_, plaintext, err := svc.Issue(ctx, "ci-bot")
	require.NoError(t, err)

	got, err := svc.Validate(ctx, plaintext)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ci-bot", got.Label)

	// last_used_at should now be set (best-effort, but in-memory store
	// is reliable here).
	tokens, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	assert.NotNil(t, tokens[0].LastUsedAt, "validate should bump last_used_at")
}

func TestAuthService_Validate_UnknownToken(t *testing.T) {
	// Validating a token that was never issued returns (nil, nil) —
	// the middleware treats that the same as a revoked token (401) so
	// we don't leak whether a guess "almost worked".
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	got, err := svc.Validate(context.Background(), "sqd_completelyrandomvaluenever-issued")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestAuthService_Validate_PrefixCheck(t *testing.T) {
	// A bearer value without the sqd_ prefix is rejected without
	// touching the store. Cheap defense against accidental leakage
	// (e.g. someone pastes a JWT into the header).
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	got, err := svc.Validate(context.Background(), "not-a-squadron-token")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestAuthService_Revoke_TokenStopsValidating(t *testing.T) {
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()

	token, plaintext, err := svc.Issue(ctx, "rotate-me")
	require.NoError(t, err)

	// Before revoke: validates.
	got, err := svc.Validate(ctx, plaintext)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Revoke and try again.
	require.NoError(t, svc.Revoke(ctx, token.ID))
	got, err = svc.Validate(ctx, plaintext)
	require.NoError(t, err)
	assert.Nil(t, got, "revoked token must not validate")
}

func TestAuthService_Revoke_Idempotent(t *testing.T) {
	// Revoking twice should not error. The UI may double-click a button
	// or two operators may revoke the same token concurrently.
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()
	token, _, err := svc.Issue(ctx, "x")
	require.NoError(t, err)
	require.NoError(t, svc.Revoke(ctx, token.ID))
	require.NoError(t, svc.Revoke(ctx, token.ID), "second revoke should be a no-op, not an error")
}

func TestAuthService_Revoke_NotFound(t *testing.T) {
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	err := svc.Revoke(context.Background(), "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestAuthService_List_NewestFirst(t *testing.T) {
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()
	for _, label := range []string{"first", "second", "third"} {
		_, _, err := svc.Issue(ctx, label)
		require.NoError(t, err)
	}
	tokens, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 3)
	assert.Equal(t, "third", tokens[0].Label, "newest should be first")
	assert.Equal(t, "first", tokens[2].Label)
}

func TestAuthService_PlaintextIsUnique(t *testing.T) {
	// Two tokens issued back-to-back must have different plaintexts.
	// 32 bytes of entropy makes collision astronomically unlikely, but
	// it's still worth a regression-safety assertion.
	svc := NewAuthService(memory.NewStore(), zap.NewNop())
	_, a, err := svc.Issue(context.Background(), "a")
	require.NoError(t, err)
	_, b, err := svc.Issue(context.Background(), "b")
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestActorFromContext_ZeroByDefault(t *testing.T) {
	a := ActorFromContext(context.Background())
	assert.True(t, a.IsZero())
}

func TestActorFromContext_RoundTrip(t *testing.T) {
	ctx := WithActor(context.Background(), AuthActor{TokenID: "id", TokenLabel: "ci-bot"})
	a := ActorFromContext(ctx)
	assert.False(t, a.IsZero())
	assert.Equal(t, "operator:ci-bot", a.String())
}

func TestAuditService_RecordPicksUpContextActor(t *testing.T) {
	// Putting an AuthActor on the context should make the audit
	// service stamp "operator:<label>" instead of the caller-supplied
	// "system" actor. This is the core wiring that turns authenticated
	// requests into attributed audit entries.
	store := memory.NewStore()
	svc := NewAuditService(store, nil, zap.NewNop())
	ctx := WithActor(context.Background(), AuthActor{TokenID: "tk", TokenLabel: "ci-bot"})

	err := svc.Record(ctx, AuditEntry{
		Actor:      AuditActorSystem, // caller passes system; context should override
		EventType:  "test.event",
		TargetType: AuditTargetAgent,
		TargetID:   "agent-x",
		Action:     "tick",
	})
	require.NoError(t, err)

	events, err := svc.List(context.Background(), AuditEventFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "operator:ci-bot", events[0].Actor, "context actor should win over the entry's Actor field")
}
