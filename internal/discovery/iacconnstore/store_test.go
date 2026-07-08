// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// storeFactory builds a fresh Store of the named backend. The test
// loop in TestStoreContract calls one of these per sub-test so the
// SQLite and memory backends share the exact same assertion body.
type storeFactory func(t *testing.T) Store

// storeBackends is the set of backends exercised by every contract
// test. Adding a new backend (e.g. a Postgres implementation) means
// dropping one entry here — no test changes elsewhere.
func storeBackends() []struct {
	name    string
	factory storeFactory
} {
	return []struct {
		name    string
		factory storeFactory
	}{
		{
			name: "sqlite",
			factory: func(t *testing.T) Store {
				t.Helper()
				dbPath := filepath.Join(t.TempDir(), "iacconnstore.db")
				store, err := NewSQLiteStore(Config{
					DBPath: dbPath,
					Logger: zap.NewNop(),
				})
				require.NoError(t, err, "NewSQLiteStore should succeed")
				t.Cleanup(func() { _ = store.Close() })
				return store
			},
		},
		{
			name: "memory",
			factory: func(t *testing.T) Store {
				t.Helper()
				return NewMemoryStore()
			},
		},
	}
}

// sampleConnection builds a syntactically valid IaCConnection with a
// non-empty CredCiphertext blob. The blob is arbitrary bytes — the
// substrate is opaque to its shape, so tests that exercise the Store
// don't need to round-trip through the AES-GCM marshal helpers.
func sampleConnection(repoFullName string) *IaCConnection {
	return &IaCConnection{
		Provider:           ProviderGitHub,
		AuthKind:           AuthKindPAT,
		RepoFullName:       repoFullName,
		DefaultBranch:      "main",
		RepoLayout:         RepoLayoutMono,
		BranchPrefix:       "squadron/rec",
		ReviewerTeamHandle: "my-org/sre",
		PlacementMap: []PlacementMapEntry{
			{Provider: "aws", ResourceKind: "eks-cluster-logging", FilePath: "modules/eks/main.tf"},
			{Provider: "aws", ResourceKind: "lambda-otel-layer", FilePath: "modules/lambda/main.tf"},
		},
		CredCiphertext: []byte("opaque-sealed-blob-for-tests"),
	}
}

func TestStoreContract(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			t.Run("Create_returns_id_and_round_trips_through_Get", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("acme/infra")
				require.NoError(t, store.Create(ctx, in))
				require.NotEmpty(t, in.ConnectionID, "Create must stamp a ConnectionID")
				require.False(t, in.CreatedAt.IsZero(), "Create must stamp CreatedAt")
				require.False(t, in.UpdatedAt.IsZero(), "Create must stamp UpdatedAt")

				out, err := store.Get(ctx, in.ConnectionID)
				require.NoError(t, err)
				require.NotNil(t, out)

				assert.Equal(t, in.ConnectionID, out.ConnectionID)
				assert.Equal(t, in.Provider, out.Provider)
				assert.Equal(t, in.AuthKind, out.AuthKind)
				assert.Equal(t, in.RepoFullName, out.RepoFullName)
				assert.Equal(t, in.DefaultBranch, out.DefaultBranch)
				assert.Equal(t, in.RepoLayout, out.RepoLayout)
				assert.Equal(t, in.BranchPrefix, out.BranchPrefix)
				assert.Equal(t, in.ReviewerTeamHandle, out.ReviewerTeamHandle)
				assert.Equal(t, in.PlacementMap, out.PlacementMap)
				assert.Equal(t, in.CredCiphertext, out.CredCiphertext)
				assert.WithinDuration(t, in.CreatedAt, out.CreatedAt, time.Second)
				assert.WithinDuration(t, in.UpdatedAt, out.UpdatedAt, time.Second)
			})

			t.Run("Create_then_List_returns_the_connection", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				a := sampleConnection("acme/infra-a")
				b := sampleConnection("acme/infra-b")
				require.NoError(t, store.Create(ctx, a))
				// Small sleep so CreatedAt differs and the
				// ordering assertion is meaningful.
				time.Sleep(2 * time.Millisecond)
				require.NoError(t, store.Create(ctx, b))

				listed, err := store.List(ctx)
				require.NoError(t, err)
				require.Len(t, listed, 2)

				assert.Equal(t, a.ConnectionID, listed[0].ConnectionID,
					"List ordering is created_at ASC; a was inserted first")
				assert.Equal(t, b.ConnectionID, listed[1].ConnectionID)
				assert.Equal(t, a.RepoFullName, listed[0].RepoFullName)
				assert.Equal(t, b.RepoFullName, listed[1].RepoFullName)
			})

			t.Run("Create_with_duplicate_repo_full_name_returns_typed_conflict_error", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				first := sampleConnection("acme/duplicated")
				require.NoError(t, store.Create(ctx, first))

				second := sampleConnection("acme/duplicated")
				err := store.Create(ctx, second)
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrConnectionConflict,
					"duplicate (provider, repo_full_name) must surface as ErrConnectionConflict")

				// The conflicting row was NOT persisted: List
				// still has exactly one row.
				listed, err := store.List(ctx)
				require.NoError(t, err)
				assert.Len(t, listed, 1)
			})

			t.Run("Delete_removes_the_row", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				conn := sampleConnection("acme/deletable")
				require.NoError(t, store.Create(ctx, conn))

				require.NoError(t, store.Delete(ctx, conn.ConnectionID))

				_, err := store.Get(ctx, conn.ConnectionID)
				assert.ErrorIs(t, err, ErrConnectionNotFound,
					"Get on deleted ID must return ErrConnectionNotFound")

				listed, err := store.List(ctx)
				require.NoError(t, err)
				assert.Empty(t, listed)

				// Idempotent: deleting again is a no-op.
				assert.NoError(t, store.Delete(ctx, conn.ConnectionID))
			})

			t.Run("UpdatePlacementMap_only_changes_placement_map_and_updates_updated_at", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				conn := sampleConnection("acme/placement-mut")
				require.NoError(t, store.Create(ctx, conn))
				originalUpdatedAt := conn.UpdatedAt

				time.Sleep(2 * time.Millisecond)

				newEntries := []PlacementMapEntry{
					{Provider: "aws", ResourceKind: "rds-pi-em", FilePath: "modules/rds/main.tf"},
					{Provider: "aws", ResourceKind: "s3-access-logging", FilePath: "modules/s3/main.tf"},
					{Provider: "aws", ResourceKind: "alb-access-logs", FilePath: "modules/alb/main.tf"},
				}
				require.NoError(t, store.UpdatePlacementMap(ctx, conn.ConnectionID, newEntries))

				got, err := store.Get(ctx, conn.ConnectionID)
				require.NoError(t, err)

				// Placement map is what we set.
				assert.Equal(t, newEntries, got.PlacementMap)

				// Everything else is untouched.
				assert.Equal(t, conn.Provider, got.Provider)
				assert.Equal(t, conn.AuthKind, got.AuthKind)
				assert.Equal(t, conn.RepoFullName, got.RepoFullName)
				assert.Equal(t, conn.DefaultBranch, got.DefaultBranch)
				assert.Equal(t, conn.RepoLayout, got.RepoLayout)
				assert.Equal(t, conn.BranchPrefix, got.BranchPrefix)
				assert.Equal(t, conn.ReviewerTeamHandle, got.ReviewerTeamHandle)
				assert.Equal(t, conn.CredCiphertext, got.CredCiphertext)
				assert.WithinDuration(t, conn.CreatedAt, got.CreatedAt, time.Second)

				// UpdatedAt must move forward.
				assert.True(t, got.UpdatedAt.After(originalUpdatedAt),
					"UpdatePlacementMap must move UpdatedAt forward; got %v vs original %v",
					got.UpdatedAt, originalUpdatedAt)
			})

			t.Run("UpdatePlacementMap_on_unknown_id_returns_not_found", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				err := store.UpdatePlacementMap(ctx, "nope-no-such-id",
					[]PlacementMapEntry{{Provider: "aws", ResourceKind: "x", FilePath: "f"}})
				assert.ErrorIs(t, err, ErrConnectionNotFound)
			})

			t.Run("Get_unknown_id_returns_not_found_error", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				_, err := store.Get(ctx, "no-such-connection-id")
				assert.ErrorIs(t, err, ErrConnectionNotFound)
			})

			t.Run("Create_with_missing_required_field_rejected", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				missing := sampleConnection("acme/missing-cred")
				missing.CredCiphertext = nil
				err := store.Create(ctx, missing)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "CredCiphertext is required")
			})

			t.Run("List_empty_store_returns_no_rows_no_error", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				listed, err := store.List(ctx)
				require.NoError(t, err)
				assert.Empty(t, listed)
			})
		})
	}
}

// TestProviderGitLab_round_trips confirms the store is provider-generic:
// a ProviderGitLab connection persists and reads back identically to a
// GitHub one, using the same row shape and the same opaque sealed-cred
// blob. No GitLab-specific column exists — GitLab is a discriminator
// value, not a schema fork.
func TestProviderGitLab_round_trips(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			in := sampleConnection("group/subgroup/infra")
			in.Provider = ProviderGitLab
			require.NoError(t, store.Create(ctx, in))
			require.NotEmpty(t, in.ConnectionID)

			out, err := store.Get(ctx, in.ConnectionID)
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, ProviderGitLab, out.Provider)
			assert.Equal(t, in.RepoFullName, out.RepoFullName)
			assert.Equal(t, in.CredCiphertext, out.CredCiphertext)

			list, err := store.List(ctx)
			require.NoError(t, err)
			require.Len(t, list, 1)
			assert.Equal(t, ProviderGitLab, list[0].Provider)
		})
	}
}

// TestProviderAllowlist_accepts_github_and_gitlab pins the connect
// handlers' validation surface: both providers are supported, and the
// empty/unset value defaults to GitHub for backward compatibility.
func TestProviderAllowlist_accepts_github_and_gitlab(t *testing.T) {
	assert.True(t, SupportedProviders[ProviderGitHub])
	assert.True(t, SupportedProviders[ProviderGitLab])
	assert.False(t, SupportedProviders["bitbucket"])

	assert.Equal(t, ProviderGitHub, NormalizeProvider(""))
	assert.Equal(t, ProviderGitHub, NormalizeProvider("GitHub"))
	assert.Equal(t, ProviderGitLab, NormalizeProvider("  GitLab "))
	assert.Equal(t, "bitbucket", NormalizeProvider("Bitbucket"))
}
