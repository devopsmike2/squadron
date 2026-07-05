// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/deploy"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_anchor_d6a_test.go — ADR 0013 D6-a isolation for the GHA
// walker. The walker lists deploy targets across all tenants under a
// system ctx, but each expected_agents row it materializes must land in
// the OWNING deploy target's tenant, not `default`.

// effectiveWriteTenant resolves the tenant a store write would PERSIST
// for the given ctx, mirroring the store's tenantScope: a system ctx
// resolves to DefaultTenant; a WithTenant ctx resolves to that tenant.
func effectiveWriteTenant(ctx context.Context) string {
	if identity.IsSystemContext(ctx) {
		return identity.DefaultTenant
	}
	return identity.TenantFromContext(ctx)
}

func d6aWalkerProvider() *fakeWalkerProvider {
	return &fakeWalkerProvider{
		runs: []deploy.WorkflowRunSummary{
			{RunID: 95, HeadSHA: "sha-95", CreatedAt: time.Now().Add(-time.Hour)},
		},
		contentsByRef: map[string]map[string][]byte{
			"sha-95": {"winOtel/ansible/inventory.ini": []byte("[windows]\nhost01\n")},
		},
	}
}

// TestGHAWalker_D6a_ExpectedAgentLandsInOwningTenant: a deploy target in
// tenant acme ⇒ the upsert ctx carries acme, not default.
func TestGHAWalker_D6a_ExpectedAgentLandsInOwningTenant(t *testing.T) {
	store := newFakeWalkerStore()
	store.targets = []*apptypes.DeployTarget{{
		ID:                  "t1",
		Name:                "acme deploy",
		InventoryPath:       "winOtel/ansible/inventory.ini",
		EncryptedCredential: []byte("not-empty"),
		TenantID:            "acme",
	}}
	walker := NewGHAWalker(store, &fakeBridge{pat: "ghp_fake"}, d6aWalkerProvider(),
		time.Hour, 30*24*time.Hour, zap.NewNop())

	// The real walker runs under a system ctx.
	require.NoError(t, walker.WalkAll(identity.WithSystemContext(context.Background())))
	require.NotEmpty(t, store.expected, "walker should have upserted at least one host")
	require.Equal(t, "acme", store.lastUpsertTenant,
		"expected_agents row landed in acme not default — anchored on the deploy target")
}

// TestGHAWalker_D6a_InertInOSS: a deploy target with no tenant (OSS
// reality) ⇒ the upsert stays default. Stamp is a no-op.
func TestGHAWalker_D6a_InertInOSS(t *testing.T) {
	store := newFakeWalkerStore()
	store.targets = []*apptypes.DeployTarget{{
		ID:                  "t1",
		Name:                "default deploy",
		InventoryPath:       "winOtel/ansible/inventory.ini",
		EncryptedCredential: []byte("not-empty"),
		// No TenantID (OSS reality).
	}}
	walker := NewGHAWalker(store, &fakeBridge{pat: "ghp_fake"}, d6aWalkerProvider(),
		time.Hour, 30*24*time.Hour, zap.NewNop())

	require.NoError(t, walker.WalkAll(identity.WithSystemContext(context.Background())))
	require.NotEmpty(t, store.expected)
	require.Equal(t, identity.DefaultTenant, store.lastUpsertTenant,
		"empty anchor → default fallback (inert in OSS)")
}
