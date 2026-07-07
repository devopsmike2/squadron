// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestTraceBudgets_CRUDRoundTrip proves ADR 0026 store breadth: Set→Get returns
// the value+ok; List reflects it; Delete makes Get ok=false; non-positive Set
// errors.
func TestTraceBudgets_CRUDRoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := s.(*Storage)
		ctx := context.Background()

		// Unset tenant → ok=false, no error.
		got, ok, err := st.GetTraceBudget(ctx, "acme")
		require.NoError(t, err)
		assert.False(t, ok, "no override yet")
		assert.Equal(t, 0, got)

		// Set then Get.
		require.NoError(t, st.SetTraceBudget(ctx, "acme", 500_000))
		got, ok, err = st.GetTraceBudget(ctx, "acme")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, 500_000, got)

		// Update via upsert.
		require.NoError(t, st.SetTraceBudget(ctx, "acme", 250_000))
		got, ok, err = st.GetTraceBudget(ctx, "acme")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, 250_000, got)

		// List reflects it.
		require.NoError(t, st.SetTraceBudget(ctx, "beta", 42))
		all, err := st.ListTraceBudgets(ctx)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"acme": 250_000, "beta": 42}, all)

		// Delete → Get ok=false.
		require.NoError(t, st.DeleteTraceBudget(ctx, "acme"))
		_, ok, err = st.GetTraceBudget(ctx, "acme")
		require.NoError(t, err)
		assert.False(t, ok, "override cleared")

		// Non-positive / empty Set errors.
		assert.Error(t, st.SetTraceBudget(ctx, "acme", 0))
		assert.Error(t, st.SetTraceBudget(ctx, "acme", -1))
		assert.Error(t, st.SetTraceBudget(ctx, "", 5))
	})
}

// TestTraceBudgets_SeedDoesNotOverwrite proves ADR 0026 D4: SeedTraceBudgets
// inserts a new tenant but does NOT overwrite an existing runtime edit.
func TestTraceBudgets_SeedDoesNotOverwrite(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := s.(*Storage)
		ctx := context.Background()

		// Runtime edit for acme.
		require.NoError(t, st.SetTraceBudget(ctx, "acme", 999))

		// Seed tries to set acme=1 (must be ignored) and gamma=7 (new insert).
		require.NoError(t, st.SeedTraceBudgets(ctx, map[string]int{"acme": 1, "gamma": 7}))

		got, ok, err := st.GetTraceBudget(ctx, "acme")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 999, got, "seed must not overwrite existing runtime edit")

		got, ok, err = st.GetTraceBudget(ctx, "gamma")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 7, got, "seed inserts a new tenant")
	})
}
