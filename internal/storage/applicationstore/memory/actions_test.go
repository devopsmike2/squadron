// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestRunnerRegistrationLifecycle covers the create / get / list /
// update / revoke flow for a single registration.
func TestRunnerRegistrationLifecycle(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	reg := &types.ActionRunnerRegistration{
		RunnerID:         "ed25519:abc",
		Hostname:         "web-prod-1",
		PublicKeyPEM:     "-----BEGIN ED25519 PUBLIC KEY-----\n...\n-----END ED25519 PUBLIC KEY-----",
		CapabilitiesJSON: `[{"type":"restart-systemd-service"}]`,
	}
	require.NoError(t, s.CreateActionRunnerRegistration(ctx, reg))
	assert.False(t, reg.RegisteredAt.IsZero(), "RegisteredAt should default to now")

	// duplicate
	err := s.CreateActionRunnerRegistration(ctx, &types.ActionRunnerRegistration{RunnerID: "ed25519:abc"})
	require.Error(t, err)

	got, err := s.GetActionRunnerRegistration(ctx, "ed25519:abc")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "web-prod-1", got.Hostname)

	// missing
	missing, err := s.GetActionRunnerRegistration(ctx, "ed25519:nope")
	require.NoError(t, err)
	assert.Nil(t, missing)

	// list
	all, err := s.ListActionRunnerRegistrations(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)

	// update
	reg.Hostname = "web-prod-2"
	reg.LastSeenAt = time.Now().UTC()
	require.NoError(t, s.UpdateActionRunnerRegistration(ctx, reg))
	got, _ = s.GetActionRunnerRegistration(ctx, "ed25519:abc")
	assert.Equal(t, "web-prod-2", got.Hostname)

	// revoke
	now := time.Now().UTC()
	require.NoError(t, s.RevokeActionRunnerRegistration(ctx, "ed25519:abc", now))
	got, _ = s.GetActionRunnerRegistration(ctx, "ed25519:abc")
	require.NotNil(t, got.RevokedAt)
	assert.WithinDuration(t, now, *got.RevokedAt, time.Second)

	// revoke missing
	assert.Error(t, s.RevokeActionRunnerRegistration(ctx, "ed25519:nope", now))
}

// TestActionRequestLifecycle exercises create + update + filter on
// the in-memory store.
func TestActionRequestLifecycle(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	r := &types.ActionRequest{
		ID:             "req-1",
		ProposalID:     "prop-1",
		RunnerID:       "ed25519:abc",
		ActionType:     "restart-systemd-service",
		ParametersJSON: `{"unit_name":"nginx"}`,
		Signature:      "sig",
		Phase:          "dry_run",
		IssuedAt:       time.Now().UTC(),
		ExpiresAt:      time.Now().Add(5 * time.Minute).UTC(),
	}
	require.NoError(t, s.CreateActionRequest(ctx, r))
	assert.Equal(t, "pending", r.Status, "CreateActionRequest must default status to pending")

	// update with a success result
	r.Status = "success"
	completed := time.Now().UTC()
	r.CompletedAt = &completed
	r.DryRunOutputJSON = `{"would_restart":"nginx"}`
	require.NoError(t, s.UpdateActionRequest(ctx, r))

	got, err := s.GetActionRequest(ctx, "req-1")
	require.NoError(t, err)
	assert.Equal(t, "success", got.Status)
	assert.Equal(t, `{"would_restart":"nginx"}`, got.DryRunOutputJSON)

	// filter by proposal_id
	list, err := s.ListActionRequests(ctx, types.ActionRequestFilter{ProposalID: "prop-1"})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// filter excludes non-matching
	list, err = s.ListActionRequests(ctx, types.ActionRequestFilter{ProposalID: "prop-other"})
	require.NoError(t, err)
	assert.Empty(t, list)

	// filter by status
	list, _ = s.ListActionRequests(ctx, types.ActionRequestFilter{Status: "success"})
	require.Len(t, list, 1)
	list, _ = s.ListActionRequests(ctx, types.ActionRequestFilter{Status: "pending"})
	assert.Empty(t, list)

	// duplicate
	err = s.CreateActionRequest(ctx, &types.ActionRequest{ID: "req-1"})
	assert.Error(t, err)

	// update missing
	err = s.UpdateActionRequest(ctx, &types.ActionRequest{ID: "req-nope"})
	assert.Error(t, err)
}

// TestActionRequestSortNewestFirst verifies List orders results by
// issued_at descending, matching the SQLite implementation.
func TestActionRequestSortNewestFirst(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	now := time.Now().UTC()
	for i, ts := range []time.Time{now.Add(-3 * time.Minute), now.Add(-1 * time.Minute), now.Add(-2 * time.Minute)} {
		require.NoError(t, s.CreateActionRequest(ctx, &types.ActionRequest{
			ID:        []string{"a", "b", "c"}[i],
			RunnerID:  "r",
			Phase:     "dry_run",
			IssuedAt:  ts,
			ExpiresAt: ts.Add(5 * time.Minute),
		}))
	}
	list, err := s.ListActionRequests(ctx, types.ActionRequestFilter{})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "b", list[0].ID)
	assert.Equal(t, "c", list[1].ID)
	assert.Equal(t, "a", list[2].ID)
}
