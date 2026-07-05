// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/ai"
)

// tenant_anchor_d6a_test.go — ADR 0013 D6-a isolation, the point of the
// fix. The proposer bridge reads open cost-spikes across all tenants
// under a system ctx, but each AI-proposed rollout must land in the
// OWNING GROUP's tenant, not `default`. These tests drive the bridge
// tick under a system ctx (mirroring the real all-in-one wiring) and
// assert the rollout-create ctx carried the group's tenant.

// effectiveWriteTenant resolves the tenant a store write would PERSIST
// for the given ctx, mirroring the store's tenantScope: a system ctx
// (no real tenant stamped) resolves to DefaultTenant; a WithTenant ctx
// resolves to that tenant. The bridges run under a system ctx, so an
// UNSTAMPED write there persists `default` (not the `__system__`
// sentinel that TenantFromContext would surface). The fakes use this so
// the assertions match what actually lands on disk.
func effectiveWriteTenant(ctx context.Context) string {
	if identity.IsSystemContext(ctx) {
		return identity.DefaultTenant
	}
	return identity.TenantFromContext(ctx)
}

// TestBridge_D6a_RolloutLandsInOwningTenant: a group in tenant acme →
// the created rollout is stamped acme, NOT default.
func TestBridge_D6a_RolloutLandsInOwningTenant(t *testing.T) {
	store, _ := baselineFixture()
	// Anchor the fixture group in tenant acme.
	store.groups["prod-utility-fleet"].TenantID = "acme"

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())

	// The real bridge runs under a system ctx (main.go wires
	// identity.WithSystemContext). Drive the tick the same way.
	b.tick(identity.WithSystemContext(context.Background()))

	require.Len(t, rollouts.inputs, 1, "one rollout proposal should have been posted")
	require.Equal(t, "acme", rollouts.lastCreateTenant,
		"rollout landed in acme not default — per-owner tenant anchored on the group")
}

// TestBridge_D6a_PlanLandsInOwningTenant: same, via the plan-create
// dispatch path.
func TestBridge_D6a_PlanLandsInOwningTenant(t *testing.T) {
	store, _ := baselineFixture()
	store.groups["prod-utility-fleet"].TenantID = "acme"

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodPlan("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{planID: "plan-acme"}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())

	b.tick(identity.WithSystemContext(context.Background()))

	require.NotEmpty(t, rollouts.planSteps, "plan create should have been invoked")
	require.Equal(t, "acme", rollouts.lastCreatePlanTenant,
		"plan rollouts landed in acme not default")
}

// TestBridge_D6a_InertInOSS: with the group in the default tenant (the
// OSS reality), the rollout still lands in default — the stamp is a
// no-op, and existing behavior is unchanged.
func TestBridge_D6a_InertInOSS(t *testing.T) {
	store, _ := baselineFixture()
	// Leave the group's TenantID empty (the OSS on-disk reality — every
	// group resolves to default).

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())

	b.tick(identity.WithSystemContext(context.Background()))

	require.Len(t, rollouts.inputs, 1)
	require.Equal(t, identity.DefaultTenant, rollouts.lastCreateTenant,
		"empty anchor → default fallback (inert in OSS)")
}
