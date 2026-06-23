// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azureconnstore

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
// Mirrors the gcpconnstore test rig.
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
				dbPath := filepath.Join(t.TempDir(), "azureconnstore.db")
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

// sampleConnection builds a syntactically valid AzureConnection with
// non-empty SealedSecret bytes. The blob is arbitrary — the
// substrate is opaque to its shape, so tests that exercise the Store
// don't need to round-trip through credstore.SealAzureClientSecret.
func sampleConnection(subscriptionID string) *AzureConnection {
	return &AzureConnection{
		DisplayName:                      "Sandbox " + subscriptionID,
		TenantID:                         "00000000-0000-0000-0000-000000000001",
		SubscriptionID:                   subscriptionID,
		ClientID:                         "00000000-0000-0000-0000-000000000002",
		SealedSecret:                     []byte("opaque-sealed-secret-blob-for-tests"),
		Location:                         "eastus",
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

				in := sampleConnection("11111111-1111-1111-1111-111111111111")
				require.NoError(t, store.Create(ctx, in))
				require.NotEmpty(t, in.ID, "Create must stamp an ID")
				require.False(t, in.CreatedAt.IsZero(), "Create must stamp CreatedAt")
				require.False(t, in.UpdatedAt.IsZero(), "Create must stamp UpdatedAt")

				out, err := store.Get(ctx, in.ID)
				require.NoError(t, err)
				require.NotNil(t, out)

				assert.Equal(t, in.ID, out.ID)
				assert.Equal(t, in.DisplayName, out.DisplayName)
				assert.Equal(t, in.TenantID, out.TenantID)
				assert.Equal(t, in.SubscriptionID, out.SubscriptionID)
				assert.Equal(t, in.ClientID, out.ClientID)
				assert.Equal(t, in.SealedSecret, out.SealedSecret,
					"Get must return the on-disk sealed bytes verbatim so the scanner can unseal")
				assert.Equal(t, in.Location, out.Location)
				assert.Equal(t, in.LearnFromAcceptedRecommendations, out.LearnFromAcceptedRecommendations)
				assert.WithinDuration(t, in.CreatedAt, out.CreatedAt, time.Second)
				assert.WithinDuration(t, in.UpdatedAt, out.UpdatedAt, time.Second)
			})

			t.Run("ListReturnsAll", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				a := sampleConnection("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
				b := sampleConnection("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
				c := sampleConnection("cccccccc-cccc-cccc-cccc-cccccccccccc")
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

				in := sampleConnection("dddddddd-dddd-dddd-dddd-dddddddddddd")
				require.NoError(t, store.Create(ctx, in))
				originalUpdatedAt := in.UpdatedAt
				originalSealedSecret := append([]byte(nil), in.SealedSecret...)

				time.Sleep(2 * time.Millisecond)

				// Update with a new DisplayName but leave SealedSecret
				// empty so the substrate keeps the existing sealed
				// bytes in place.
				patch := &AzureConnection{
					ID:                               in.ID,
					DisplayName:                      "Sandbox renamed",
					TenantID:                         in.TenantID,
					SubscriptionID:                   in.SubscriptionID,
					ClientID:                         in.ClientID,
					Location:                         in.Location,
					LearnFromAcceptedRecommendations: in.LearnFromAcceptedRecommendations,
					// SealedSecret intentionally left nil — substrate
					// preserves the existing sealed bytes.
				}
				require.NoError(t, store.Update(ctx, patch))

				got, err := store.Get(ctx, in.ID)
				require.NoError(t, err)

				assert.Equal(t, "Sandbox renamed", got.DisplayName, "DisplayName updated")
				assert.Equal(t, in.SubscriptionID, got.SubscriptionID, "SubscriptionID unchanged")
				assert.Equal(t, originalSealedSecret, got.SealedSecret,
					"empty SealedSecret in Update must leave the existing sealed bytes in place")
				assert.True(t, got.UpdatedAt.After(originalUpdatedAt),
					"Update must move UpdatedAt forward; got %v vs original %v",
					got.UpdatedAt, originalUpdatedAt)
				assert.WithinDuration(t, in.CreatedAt, got.CreatedAt, time.Second,
					"CreatedAt must NOT change on Update")
			})

			t.Run("Delete_RemovesRow", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				conn := sampleConnection("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
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

				patch := sampleConnection("ffffffff-ffff-ffff-ffff-ffffffffffff")
				patch.ID = "nope-no-such-id"
				err := store.Update(ctx, patch)
				assert.ErrorIs(t, err, ErrConnectionNotFound)
			})

			t.Run("Create_with_missing_required_field_rejected", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				missing := sampleConnection("11111111-2222-3333-4444-555555555555")
				missing.SealedSecret = nil
				err := store.Create(ctx, missing)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "SealedSecret is required")
			})

			t.Run("List_empty_store_returns_no_rows_no_error", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				listed, err := store.List(ctx)
				require.NoError(t, err)
				assert.Empty(t, listed)
			})

			t.Run("Location_empty_string_means_scan_all", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("99999999-9999-9999-9999-999999999999")
				in.Location = "" // explicitly empty
				require.NoError(t, store.Create(ctx, in))

				out, err := store.Get(ctx, in.ID)
				require.NoError(t, err)
				assert.Equal(t, "", out.Location,
					"empty Location must round-trip as empty (scan-all sentinel)")
			})
		})
	}
}

// TestSealedSecretNeverInListResponse — v0.89.51 (#674 slice 1 chunk
// 1). The AzureConnection struct's SealedSecret field carries the
// json:"-" tag, which the encoding/json marshaler must respect on
// every code path. This is the defense-in-depth check the threat
// model (§12) names: even if a future handler accidentally marshals
// an AzureConnection directly to an HTTP response body, the sealed
// bytes do NOT leak.
//
// The reflection check pins the json tag literally so a refactor
// that renames the field cannot silently drop the suppression.
// Mirrors gcpconnstore.TestSealedSANeverInListResponse.
func TestAzureConnectionStore_SealedSecretNeverInListResponse(t *testing.T) {
	t.Parallel()

	// (1) Reflection: confirm the field exists and the JSON tag is
	// exactly "-".
	typ := reflect.TypeOf(AzureConnection{})
	field, ok := typ.FieldByName("SealedSecret")
	require.True(t, ok, "SealedSecret field must exist on AzureConnection")
	gotTag := field.Tag.Get("json")
	if gotTag != "-" {
		t.Errorf("SealedSecret json tag = %q, want \"-\" (this is the security boundary that keeps sealed bytes off every HTTP marshal path)", gotTag)
	}

	// (2) Programmatic check: a sample AzureConnection with
	// non-trivial SealedSecret bytes must marshal to JSON with no
	// trace of the sealed bytes. We use a non-printable but
	// recognizable pattern so any leak surfaces as a hex / base64
	// substring match against any of the obvious encodings
	// json.Marshal might pick.
	const canary = "SEALED-AZURE-SECRET-MUST-NEVER-APPEAR-IN-JSON"
	conn := AzureConnection{
		ID:             "abc-123",
		DisplayName:    "Sandbox",
		TenantID:       "00000000-0000-0000-0000-000000000001",
		SubscriptionID: "00000000-0000-0000-0000-000000000010",
		ClientID:       "00000000-0000-0000-0000-000000000020",
		SealedSecret:   []byte(canary),
		Location:       "eastus",
	}
	raw, err := json.Marshal(conn)
	require.NoError(t, err)
	if strings.Contains(string(raw), canary) {
		t.Errorf("SealedSecret canary leaked into JSON marshal output: %s", string(raw))
	}
	// Also check the obvious base64 encoding json.Marshal uses for
	// []byte fields when no tag suppresses it.
	if strings.Contains(string(raw), "U0VBTEVE") { // base64 prefix of "SEALED"
		t.Errorf("SealedSecret bytes leaked into JSON marshal output (base64-encoded): %s", string(raw))
	}
}
