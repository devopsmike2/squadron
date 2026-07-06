// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/extension/tracebudget"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// TestTraceResource_PerTenantEviction_IsolatesTenants proves ADR 0024: a tenant
// over its budget evicts only its OWN oldest rows — another tenant's rows,
// including one sharing the same resource_key, are untouched.
func TestTraceResource_PerTenantEviction_IsolatesTenants(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		s.(*Storage).traceIndexMaxRow = 2
		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
		acme := identity.WithTenant(context.Background(), "acme")
		beta := identity.WithTenant(context.Background(), "beta")

		// beta holds "shared" first (1 row, under budget).
		_, err := s.UpsertTraceResources(beta, []traceindex.ResourceRow{
			makeTraceRow("shared", now, 1),
		})
		require.NoError(t, err)

		// acme goes over budget (3 rows); its OLDEST ("shared") is evicted.
		evicted, err := s.UpsertTraceResources(acme, []traceindex.ResourceRow{
			makeTraceRow("shared", now.Add(-2*time.Hour), 1),
			makeTraceRow("a2", now.Add(-1*time.Hour), 1),
			makeTraceRow("a3", now, 1),
		})
		require.NoError(t, err)
		assert.Equal(t, 1, evicted, "acme evicts exactly its oldest")

		// acme's "shared" gone; a2/a3 remain.
		got, err := s.GetTraceResource(acme, "shared")
		require.NoError(t, err)
		assert.Nil(t, got, "acme oldest evicted")
		got, err = s.GetTraceResource(acme, "a3")
		require.NoError(t, err)
		assert.NotNil(t, got)

		// beta's "shared" — same resource_key — SURVIVES acme's eviction.
		got, err = s.GetTraceResource(beta, "shared")
		require.NoError(t, err)
		assert.NotNil(t, got, "beta's row must NOT be evicted by acme's sweep (tenant isolation)")
	})
}

// TestTraceResource_PerTenantBudgetProvider proves a per-tenant budget override:
// acme gets a larger budget (not evicted at 3), beta falls back to the global cap.
func TestTraceResource_PerTenantBudgetProvider(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := s.(*Storage)
		st.traceIndexMaxRow = 2
		st.SetTraceBudgetProvider(tracebudget.NewMapProvider(map[string]int{"acme": 5}))

		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
		acme := identity.WithTenant(context.Background(), "acme")
		beta := identity.WithTenant(context.Background(), "beta")

		// acme: 3 rows, per-tenant budget 5 → no eviction.
		ev, err := s.UpsertTraceResources(acme, []traceindex.ResourceRow{
			makeTraceRow("x1", now.Add(-2*time.Hour), 1),
			makeTraceRow("x2", now.Add(-1*time.Hour), 1),
			makeTraceRow("x3", now, 1),
		})
		require.NoError(t, err)
		assert.Equal(t, 0, ev, "acme under its per-tenant budget → no eviction")
		got, err := s.GetTraceResource(acme, "x1")
		require.NoError(t, err)
		assert.NotNil(t, got, "acme keeps all 3 (budget 5)")

		// beta: 3 rows, no override → global cap 2 → evict 1.
		ev, err = s.UpsertTraceResources(beta, []traceindex.ResourceRow{
			makeTraceRow("y1", now.Add(-2*time.Hour), 1),
			makeTraceRow("y2", now.Add(-1*time.Hour), 1),
			makeTraceRow("y3", now, 1),
		})
		require.NoError(t, err)
		assert.Equal(t, 1, ev, "beta uses the global cap 2 → evicts 1")
	})
}
