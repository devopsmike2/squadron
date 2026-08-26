// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestAuditTenantCommingling_SQLite proves the ADR 0043 operator preflight on
// the SQLite backend: an empty/single-tenant store is safe to arm, and two
// distinct tenants in one table are flagged commingled + unsafe. Runs in the
// always-on Go Backend job (no Postgres needed).
func TestAuditTenantCommingling_SQLite(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		system := identity.WithSystemContext(context.Background())

		// Single tenant → safe.
		acme := identity.WithTenant(context.Background(), "acme")
		require.NoError(t, store.CreateAgent(acme, makeTestAgent(uuid.New())))
		rep, err := store.AuditTenantCommingling(system)
		require.NoError(t, err)
		require.False(t, rep.AnyCommingled, "single tenant must not be commingled")
		require.True(t, rep.SafeToArmStrict())

		// Add a second tenant to agents → commingled.
		globex := identity.WithTenant(context.Background(), "globex")
		require.NoError(t, store.CreateAgent(globex, makeTestAgent(uuid.New())))
		rep, err = store.AuditTenantCommingling(system)
		require.NoError(t, err)
		require.True(t, rep.AnyCommingled, "two tenants in agents must be flagged")
		require.False(t, rep.SafeToArmStrict())

		var agents *types.TenantComminglingTableReport
		for i := range rep.Tables {
			if rep.Tables[i].Table == "agents" {
				agents = &rep.Tables[i]
			}
		}
		require.NotNil(t, agents)
		require.Equal(t, int64(2), agents.DistinctTenants)
		require.True(t, agents.Commingled)
	})
}
