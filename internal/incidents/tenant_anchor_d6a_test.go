// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package incidents

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_anchor_d6a_test.go — ADR 0013 D6-a isolation for the incidents
// bridge. The drafter polls completed action requests across all tenants
// under a system ctx, but each drafted incident must land in the OWNING
// GROUP's tenant (reached via the originating rollout), not `default`.

// effectiveWriteTenant resolves the tenant a store write would PERSIST
// for the given ctx, mirroring the store's tenantScope: a system ctx
// resolves to DefaultTenant; a WithTenant ctx resolves to that tenant.
func effectiveWriteTenant(ctx context.Context) string {
	if identity.IsSystemContext(ctx) {
		return identity.DefaultTenant
	}
	return identity.TenantFromContext(ctx)
}

// TestBridge_D6a_DraftLandsInOwningTenant: action → rollout → group in
// tenant acme ⇒ the incident draft is stamped acme, not default.
func TestBridge_D6a_DraftLandsInOwningTenant(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", now.Add(-time.Minute))
	// The owning group is in tenant acme.
	store.groups["web-group"] = &types.Group{ID: "web-group", Name: "Web Group", TenantID: "acme"}

	rollouts := &fakeRollouts{rollouts: map[string]*services.Rollout{
		"rollout-a1": {ID: "rollout-a1", Name: "AI: pin hashing.rounds=6", GroupID: "web-group"},
	}}
	drafter := &fakeDrafter{enabled: true}
	audit := &fakeAudit{}

	b, err := New(drafter, store, rollouts, audit, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })

	// The real bridge runs under a system ctx (main.go wires
	// identity.WithSystemContext). Drive the tick the same way.
	b.Tick(identity.WithSystemContext(context.Background()))

	require.Len(t, store.drafts, 1)
	require.Equal(t, "acme", store.lastCreateTenant,
		"draft landed in acme not default — per-owner tenant anchored on the group")
}

// TestBridge_D6a_InertInOSS: group in the default tenant (the OSS
// reality) ⇒ the draft still lands in default. Stamp is a no-op.
func TestBridge_D6a_InertInOSS(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", now.Add(-time.Minute))
	// Group carries no tenant (OSS reality).
	store.groups["web-group"] = &types.Group{ID: "web-group", Name: "Web Group"}

	rollouts := &fakeRollouts{rollouts: map[string]*services.Rollout{
		"rollout-a1": {ID: "rollout-a1", Name: "n", GroupID: "web-group"},
	}}
	drafter := &fakeDrafter{enabled: true}
	audit := &fakeAudit{}

	b, err := New(drafter, store, rollouts, audit, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(identity.WithSystemContext(context.Background()))

	require.Len(t, store.drafts, 1)
	require.Equal(t, identity.DefaultTenant, store.lastCreateTenant,
		"empty anchor → default fallback (inert in OSS)")
}

// TestBridge_D6a_NoRolloutLeavesDefault: an action with no originating
// rollout (ProposalID == "") has no group anchor ⇒ the draft stays
// default (the legit fallback documented in the ADR).
func TestBridge_D6a_NoRolloutLeavesDefault(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	req := completedRequest("a1", "success", now.Add(-time.Minute))
	req.ProposalID = "" // no originating rollout → no group anchor
	store.requests["a1"] = req

	drafter := &fakeDrafter{enabled: true}
	audit := &fakeAudit{}
	// No rollouts service wired: the group can't be reached anyway.
	b, err := New(drafter, store, nil, audit, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(identity.WithSystemContext(context.Background()))

	require.Len(t, store.drafts, 1)
	require.Equal(t, identity.DefaultTenant, store.lastCreateTenant,
		"no rollout → no group anchor → default (legit fallback)")
}
