// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/identity"
)

// TestTenantScope covers the four resolution paths of the ADR 0011 M2
// helper: system context (no predicate), an explicitly stamped tenant, an
// unstamped context with strict scoping off (legacy DefaultTenant
// fallback), and an unstamped context with strict scoping on (fail-fast).
func TestTenantScope(t *testing.T) {
	t.Run("system context applies no predicate", func(t *testing.T) {
		ctx := identity.WithSystemContext(context.Background())
		tenant, apply, err := tenantScope(ctx)
		require.NoError(t, err)
		require.False(t, apply, "system context must not apply a tenant predicate")
		require.Equal(t, "", tenant)
	})

	t.Run("stamped tenant scopes to that tenant", func(t *testing.T) {
		ctx := identity.WithTenant(context.Background(), "acme")
		tenant, apply, err := tenantScope(ctx)
		require.NoError(t, err)
		require.True(t, apply)
		require.Equal(t, "acme", tenant)
	})

	t.Run("unstamped + strict off falls back to DefaultTenant", func(t *testing.T) {
		// Guard: strict must be off (the OSS default) for this case.
		require.False(t, strictTenantScoping)
		tenant, apply, err := tenantScope(context.Background())
		require.NoError(t, err)
		require.True(t, apply)
		require.Equal(t, identity.DefaultTenant, tenant)
	})

	t.Run("unstamped + strict on errors", func(t *testing.T) {
		SetStrictTenantScoping(true)
		defer SetStrictTenantScoping(false)
		_, apply, err := tenantScope(context.Background())
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrTenantContextRequired))
		require.False(t, apply)
	})
}
