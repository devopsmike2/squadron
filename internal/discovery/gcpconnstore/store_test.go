// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// storeFactory builds a fresh Store of the named backend. The test
// loop in TestStoreContract calls one of these per sub-test so the
// SQLite and memory backends share the exact same assertion body.
// Mirrors the iacconnstore test rig.
type storeFactory func(t *testing.T) Store

// storeBackends is the set of backends exercised by every contract
// test. Adding a new backend means dropping one entry here — no test
// changes elsewhere.
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
				dbPath := filepath.Join(t.TempDir(), "gcpconnstore.db")
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

// sampleConnection builds a syntactically valid GCPConnection with a
// non-empty SealedSA blob. The blob is arbitrary bytes — the
// substrate is opaque to its shape, so tests that exercise the Store
// don't need to round-trip through credstore.SealGCPServiceAccount.
func sampleConnection(projectID string) *GCPConnection {
	return &GCPConnection{
		DisplayName:                      "Sandbox " + projectID,
		ProjectID:                        projectID,
		SealedSA:                         []byte("opaque-sealed-sa-blob-for-tests"),
		Region:                           "us-central1",
		LearnFromAcceptedRecommendations: true,
	}
}

func TestStoreContract(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			t.Run("CreateAndGetRoundTrip", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("acme-prod")
				require.NoError(t, store.Create(ctx, in))
				require.NotEmpty(t, in.ID, "Create must stamp an ID")
				require.False(t, in.CreatedAt.IsZero(), "Create must stamp CreatedAt")
				require.False(t, in.UpdatedAt.IsZero(), "Create must stamp UpdatedAt")

				out, err := store.Get(ctx, in.ID)
				require.NoError(t, err)
				require.NotNil(t, out)

				assert.Equal(t, in.ID, out.ID)
				assert.Equal(t, in.DisplayName, out.DisplayName)
				assert.Equal(t, in.ProjectID, out.ProjectID)
				assert.Equal(t, in.SealedSA, out.SealedSA,
					"Get must return the on-disk sealed SA bytes verbatim so the scanner can unseal")
				assert.Equal(t, in.Region, out.Region)
				assert.Equal(t, in.LearnFromAcceptedRecommendations, out.LearnFromAcceptedRecommendations)
				assert.WithinDuration(t, in.CreatedAt, out.CreatedAt, time.Second)
				assert.WithinDuration(t, in.UpdatedAt, out.UpdatedAt, time.Second)
			})

			t.Run("ListReturnsAll", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				a := sampleConnection("acme-a")
				b := sampleConnection("acme-b")
				c := sampleConnection("acme-c")
				require.NoError(t, store.Create(ctx, a))
				time.Sleep(2 * time.Millisecond)
				require.NoError(t, store.Create(ctx, b))
				time.Sleep(2 * time.Millisecond)
				require.NoError(t, store.Create(ctx, c))

				listed, err := store.List(ctx)
				require.NoError(t, err)
				require.Len(t, listed, 3)

				// Ordering: created_at ASC. a was inserted first.
				assert.Equal(t, a.ID, listed[0].ID)
				assert.Equal(t, b.ID, listed[1].ID)
				assert.Equal(t, c.ID, listed[2].ID)
			})

			t.Run("UpdateModifiesDisplayName", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("acme-mut")
				require.NoError(t, store.Create(ctx, in))
				originalUpdatedAt := in.UpdatedAt
				originalSealedSA := append([]byte(nil), in.SealedSA...)

				time.Sleep(2 * time.Millisecond)

				// Update with a new DisplayName but leave SealedSA
				// empty so the substrate keeps the existing sealed
				// bytes in place.
				patch := &GCPConnection{
					ID:                               in.ID,
					DisplayName:                      "Sandbox renamed",
					ProjectID:                        in.ProjectID,
					Region:                           in.Region,
					LearnFromAcceptedRecommendations: in.LearnFromAcceptedRecommendations,
					// SealedSA intentionally left nil — substrate
					// preserves the existing sealed bytes.
				}
				require.NoError(t, store.Update(ctx, patch))

				got, err := store.Get(ctx, in.ID)
				require.NoError(t, err)

				assert.Equal(t, "Sandbox renamed", got.DisplayName, "DisplayName updated")
				assert.Equal(t, in.ProjectID, got.ProjectID, "ProjectID unchanged")
				assert.Equal(t, originalSealedSA, got.SealedSA,
					"empty SealedSA in Update must leave the existing sealed bytes in place")
				assert.True(t, got.UpdatedAt.After(originalUpdatedAt),
					"Update must move UpdatedAt forward; got %v vs original %v",
					got.UpdatedAt, originalUpdatedAt)
				assert.WithinDuration(t, in.CreatedAt, got.CreatedAt, time.Second,
					"CreatedAt must NOT change on Update")
			})

			t.Run("Delete_RemovesRow", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				conn := sampleConnection("acme-deletable")
				require.NoError(t, store.Create(ctx, conn))

				require.NoError(t, store.Delete(ctx, conn.ID))

				_, err := store.Get(ctx, conn.ID)
				assert.ErrorIs(t, err, ErrConnectionNotFound,
					"Get on deleted ID must return ErrConnectionNotFound")

				listed, err := store.List(ctx)
				require.NoError(t, err)
				assert.Empty(t, listed)

				// Idempotent: deleting again is a no-op.
				assert.NoError(t, store.Delete(ctx, conn.ID))
			})

			t.Run("GetMissing_ReturnsNotFoundError", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				_, err := store.Get(ctx, "no-such-id")
				assert.ErrorIs(t, err, ErrConnectionNotFound)
			})

			t.Run("Update_on_unknown_id_returns_not_found", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				patch := sampleConnection("acme-orphan")
				patch.ID = "nope-no-such-id"
				err := store.Update(ctx, patch)
				assert.ErrorIs(t, err, ErrConnectionNotFound)
			})

			t.Run("Create_with_missing_required_field_rejected", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				missing := sampleConnection("acme-missing-sa")
				missing.SealedSA = nil
				err := store.Create(ctx, missing)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "SealedSA is required")
			})

			t.Run("List_empty_store_returns_no_rows_no_error", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				listed, err := store.List(ctx)
				require.NoError(t, err)
				assert.Empty(t, listed)
			})

			t.Run("Region_empty_string_means_scan_all", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("acme-scan-all")
				in.Region = "" // explicitly empty
				require.NoError(t, store.Create(ctx, in))

				out, err := store.Get(ctx, in.ID)
				require.NoError(t, err)
				assert.Equal(t, "", out.Region,
					"empty Region must round-trip as empty (scan-all sentinel)")
			})
		})
	}
}

// TestSealedSANeverInListResponse — v0.89.46 (#667 slice 1 chunk 1).
// The GCPConnection struct's SealedSA field carries the json:"-" tag,
// which the encoding/json marshaler must respect on every code path.
// This is the defense-in-depth check the threat model (§11.1) names:
// even if a future handler accidentally marshals a GCPConnection
// directly to an HTTP response body, the sealed bytes do NOT leak.
//
// The reflection check pins the json tag literally so a refactor
// that renames the field cannot silently drop the suppression.
func TestSealedSANeverInListResponse(t *testing.T) {
	t.Parallel()

	// (1) Reflection: confirm the field exists and the JSON tag is
	// exactly "-".
	typ := reflect.TypeOf(GCPConnection{})
	field, ok := typ.FieldByName("SealedSA")
	require.True(t, ok, "SealedSA field must exist on GCPConnection")
	gotTag := field.Tag.Get("json")
	if gotTag != "-" {
		t.Errorf("SealedSA json tag = %q, want \"-\" (this is the security boundary that keeps sealed bytes off every HTTP marshal path)", gotTag)
	}

	// (2) Programmatic check: a sample GCPConnection with non-trivial
	// SealedSA bytes must marshal to JSON with no trace of the sealed
	// bytes. We use a non-printable but recognizable pattern so any
	// leak surfaces as a hex / base64 substring match against any of
	// the obvious encodings json.Marshal might pick.
	const canary = "SEALED-SA-MUST-NEVER-APPEAR-IN-JSON"
	conn := GCPConnection{
		ID:          "abc-123",
		DisplayName: "Sandbox",
		ProjectID:   "my-proj",
		SealedSA:    []byte(canary),
		Region:      "us-central1",
	}
	raw, err := json.Marshal(conn)
	require.NoError(t, err)
	if strings.Contains(string(raw), canary) {
		t.Errorf("SealedSA canary leaked into JSON marshal output: %s", string(raw))
	}
	// Also check the obvious base64 encoding json.Marshal uses for
	// []byte fields when no tag suppresses it.
	if strings.Contains(string(raw), "U0VBTEVE") { // base64 prefix of "SEALED"
		t.Errorf("SealedSA bytes leaked into JSON marshal output (base64-encoded): %s", string(raw))
	}
}
