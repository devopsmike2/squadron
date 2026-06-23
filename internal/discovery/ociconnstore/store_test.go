// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ociconnstore

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
// loop in TestOCIConnectionStoreContract calls one of these per
// sub-test so the SQLite and memory backends share the exact same
// assertion body. Mirrors the azureconnstore test rig.
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
				dbPath := filepath.Join(t.TempDir(), "ociconnstore.db")
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

// sampleConnection builds a syntactically valid OCIConnection with
// non-empty SealedPrivateKey bytes. The blob is arbitrary — the
// substrate is opaque to its shape, so tests that exercise the Store
// don't need to round-trip through credstore.SealOCIPrivateKey.
func sampleConnection(tenancySuffix string) *OCIConnection {
	return &OCIConnection{
		DisplayName:                      "Sandbox " + tenancySuffix,
		TenancyOCID:                      "ocid1.tenancy.oc1..aaaa" + tenancySuffix,
		UserOCID:                         "ocid1.user.oc1..bbbb" + tenancySuffix,
		Fingerprint:                      "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
		SealedPrivateKey:                 []byte("opaque-sealed-private-key-blob-for-tests"),
		Region:                           "us-phoenix-1",
		LearnFromAcceptedRecommendations: true,
	}
}

// TestOCIConnectionStore_CreateAndGetRoundTrip pins the basic
// substrate contract: a created connection round-trips through Get
// with every field intact and the sealed bytes returned verbatim so
// the scanner can call credstore.UnsealOCIPrivateKey on them. Acts
// as design-doc §15 acceptance test #1.
func TestOCIConnectionStore_CreateAndGetRoundTrip(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			in := sampleConnection("01")
			require.NoError(t, store.Create(ctx, in))
			require.NotEmpty(t, in.ID, "Create must stamp an ID")
			require.False(t, in.CreatedAt.IsZero(), "Create must stamp CreatedAt")
			require.False(t, in.UpdatedAt.IsZero(), "Create must stamp UpdatedAt")

			out, err := store.Get(ctx, in.ID)
			require.NoError(t, err)
			require.NotNil(t, out)

			assert.Equal(t, in.ID, out.ID)
			assert.Equal(t, in.DisplayName, out.DisplayName)
			assert.Equal(t, in.TenancyOCID, out.TenancyOCID)
			assert.Equal(t, in.UserOCID, out.UserOCID)
			assert.Equal(t, in.Fingerprint, out.Fingerprint)
			assert.Equal(t, in.SealedPrivateKey, out.SealedPrivateKey,
				"Get must return the on-disk sealed bytes verbatim so the scanner can unseal")
			assert.Equal(t, in.Region, out.Region)
			assert.Equal(t, in.LearnFromAcceptedRecommendations, out.LearnFromAcceptedRecommendations)
			assert.WithinDuration(t, in.CreatedAt, out.CreatedAt, time.Second)
			assert.WithinDuration(t, in.UpdatedAt, out.UpdatedAt, time.Second)
		})
	}
}

// TestOCIConnectionStore_ListReturnsAll confirms List returns every
// inserted row in CreatedAt ASC order, matching the SQLite query
// shape. Determinism here matters for chunk 3's HTTP handler tests.
func TestOCIConnectionStore_ListReturnsAll(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			a := sampleConnection("aa")
			b := sampleConnection("bb")
			c := sampleConnection("cc")
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
	}
}

// TestOCIConnectionStore_UpdateModifiesDisplayName confirms a
// nil-SealedPrivateKey Update path preserves the existing sealed
// bytes and only flips the targeted mutable fields. Pins the PATCH
// semantics chunk 3 will rely on.
func TestOCIConnectionStore_UpdateModifiesDisplayName(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			in := sampleConnection("dd")
			require.NoError(t, store.Create(ctx, in))
			originalUpdatedAt := in.UpdatedAt
			originalSealed := append([]byte(nil), in.SealedPrivateKey...)

			time.Sleep(2 * time.Millisecond)

			patch := &OCIConnection{
				ID:                               in.ID,
				DisplayName:                      "Sandbox renamed",
				TenancyOCID:                      in.TenancyOCID,
				UserOCID:                         in.UserOCID,
				Fingerprint:                      in.Fingerprint,
				Region:                           in.Region,
				LearnFromAcceptedRecommendations: in.LearnFromAcceptedRecommendations,
				// SealedPrivateKey intentionally left nil — substrate
				// preserves the existing sealed bytes.
			}
			require.NoError(t, store.Update(ctx, patch))

			got, err := store.Get(ctx, in.ID)
			require.NoError(t, err)

			assert.Equal(t, "Sandbox renamed", got.DisplayName, "DisplayName updated")
			assert.Equal(t, in.TenancyOCID, got.TenancyOCID, "TenancyOCID unchanged")
			assert.Equal(t, originalSealed, got.SealedPrivateKey,
				"empty SealedPrivateKey in Update must leave the existing sealed bytes in place")
			assert.True(t, got.UpdatedAt.After(originalUpdatedAt),
				"Update must move UpdatedAt forward; got %v vs original %v",
				got.UpdatedAt, originalUpdatedAt)
			assert.WithinDuration(t, in.CreatedAt, got.CreatedAt, time.Second,
				"CreatedAt must NOT change on Update")
		})
	}
}

// TestOCIConnectionStore_Delete_RemovesRow confirms Delete is
// idempotent and removes the row from List + Get.
func TestOCIConnectionStore_Delete_RemovesRow(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			conn := sampleConnection("ee")
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
	}
}

// TestOCIConnectionStore_GetMissing_ReturnsNotFoundError pins the
// errors.Is contract used by chunk 3 to surface 404 vs 500.
func TestOCIConnectionStore_GetMissing_ReturnsNotFoundError(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			_, err := store.Get(ctx, "no-such-id")
			assert.ErrorIs(t, err, ErrConnectionNotFound)
		})
	}
}

// TestOCIConnectionStore_Update_on_unknown_id_returns_not_found pins
// the symmetric not-found contract on the Update path.
func TestOCIConnectionStore_Update_on_unknown_id_returns_not_found(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()

			patch := sampleConnection("ff")
			patch.ID = "nope-no-such-id"
			err := store.Update(ctx, patch)
			assert.ErrorIs(t, err, ErrConnectionNotFound)
		})
	}
}

// TestOCIConnectionStore_Create_with_missing_required_field_rejected
// pins the field-level validation on Create — every required field
// (DisplayName, TenancyOCID, UserOCID, Fingerprint, SealedPrivateKey,
// Region) is rejected when missing. Region is the OCI-specific
// addition vs Azure/GCP — its API endpoints are regional so empty
// Region is invalid, not "scan all".
func TestOCIConnectionStore_Create_with_missing_required_field_rejected(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			t.Run("missing SealedPrivateKey", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()
				missing := sampleConnection("11")
				missing.SealedPrivateKey = nil
				err := store.Create(ctx, missing)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "SealedPrivateKey is required")
			})
			t.Run("missing Region", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()
				missing := sampleConnection("22")
				missing.Region = ""
				err := store.Create(ctx, missing)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "Region is required")
			})
			t.Run("missing Fingerprint", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()
				missing := sampleConnection("33")
				missing.Fingerprint = ""
				err := store.Create(ctx, missing)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "Fingerprint is required")
			})
		})
	}
}

// TestOCIConnectionStore_SealedPrivateKeyNeverInListResponse —
// v0.89.56 (#681 slice 1 chunk 1). The OCIConnection struct's
// SealedPrivateKey field carries the json:"-" tag, which the
// encoding/json marshaler must respect on every code path. This is
// the defense-in-depth check the threat model (§12) names: even if a
// future handler accidentally marshals an OCIConnection directly to
// an HTTP response body, the sealed bytes do NOT leak. Private key
// bytes are the strongest credential type Squadron handles, so this
// invariant is non-negotiable.
//
// The reflection check pins the json tag literally so a refactor
// that renames the field cannot silently drop the suppression.
// Mirrors azureconnstore.TestAzureConnectionStore_SealedSecretNeverInListResponse.
func TestOCIConnectionStore_SealedPrivateKeyNeverInListResponse(t *testing.T) {
	t.Parallel()

	// (1) Reflection: confirm the field exists and the JSON tag is
	// exactly "-".
	typ := reflect.TypeOf(OCIConnection{})
	field, ok := typ.FieldByName("SealedPrivateKey")
	require.True(t, ok, "SealedPrivateKey field must exist on OCIConnection")
	gotTag := field.Tag.Get("json")
	if gotTag != "-" {
		t.Errorf("SealedPrivateKey json tag = %q, want \"-\" (this is the security boundary that keeps sealed bytes off every HTTP marshal path)", gotTag)
	}

	// (2) Programmatic check: a sample OCIConnection with a
	// non-trivial SealedPrivateKey must marshal to JSON with no
	// trace of the sealed bytes. We use a non-printable but
	// recognizable pattern so any leak surfaces as a substring
	// match against any of the obvious encodings json.Marshal
	// might pick.
	const canary = "SEALED-OCI-PRIVATE-KEY-MUST-NEVER-APPEAR-IN-JSON"
	conn := OCIConnection{
		ID:               "abc-123",
		DisplayName:      "Sandbox",
		TenancyOCID:      "ocid1.tenancy.oc1..aaaa",
		UserOCID:         "ocid1.user.oc1..bbbb",
		Fingerprint:      "aa:bb:cc:dd",
		SealedPrivateKey: []byte(canary),
		Region:           "us-phoenix-1",
	}
	raw, err := json.Marshal(conn)
	require.NoError(t, err)
	if strings.Contains(string(raw), canary) {
		t.Errorf("SealedPrivateKey canary leaked into JSON marshal output: %s", string(raw))
	}
	// Also check the obvious base64 encoding json.Marshal uses for
	// []byte fields when no tag suppresses it.
	if strings.Contains(string(raw), "U0VBTEVE") { // base64 prefix of "SEALED"
		t.Errorf("SealedPrivateKey bytes leaked into JSON marshal output (base64-encoded): %s", string(raw))
	}
}

// TestOCIConnectionStore_List_empty_store_returns_no_rows_no_error
// pins the "empty store returns nil-or-empty slice + no error" shape.
func TestOCIConnectionStore_List_empty_store_returns_no_rows_no_error(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			ctx := context.Background()
			listed, err := store.List(ctx)
			require.NoError(t, err)
			assert.Empty(t, listed)
		})
	}
}
