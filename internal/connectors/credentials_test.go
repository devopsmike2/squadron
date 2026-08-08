// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// testKey builds a deterministic credstore.Key for the credential tests
// without touching the SQUADRON_SECRETS_KEY env var.
func testKey(t *testing.T) *credstore.Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	key, err := credstore.NewKey(raw)
	require.NoError(t, err)
	return key
}

// TestMarshalUnmarshalRoundTrip covers the seal/unseal round-trip of the
// credential blob.
func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	key := testKey(t)
	creds := ConnectorCredentials{
		Token:    "super-secret-token",
		Username: "svc",
		Password: "hunter2",
		Headers:  map[string]string{"DD-APPLICATION-KEY": "app-key-value"},
	}

	blob, err := MarshalCredentials(creds, key)
	require.NoError(t, err)

	got, err := UnmarshalCredentials(blob, key)
	require.NoError(t, err)
	assert.Equal(t, creds, *got)
}

// TestCredentialsEncryptedAtRest asserts the sealed blob does not
// contain any plaintext secret bytes — the encrypted-at-rest contract.
func TestCredentialsEncryptedAtRest(t *testing.T) {
	key := testKey(t)
	creds := ConnectorCredentials{
		Token:    "super-secret-token",
		Password: "hunter2",
		Headers:  map[string]string{"x-api": "app-key-value"},
	}

	blob, err := MarshalCredentials(creds, key)
	require.NoError(t, err)

	assert.False(t, bytes.Contains(blob, []byte("super-secret-token")), "token must not appear in sealed blob")
	assert.False(t, bytes.Contains(blob, []byte("hunter2")), "password must not appear in sealed blob")
	assert.False(t, bytes.Contains(blob, []byte("app-key-value")), "header value must not appear in sealed blob")
}

// TestUnmarshalWrongKeyFails asserts a blob sealed under one key cannot
// be opened with another, and the error carries the "decrypt" signal
// without leaking secret bytes.
func TestUnmarshalWrongKeyFails(t *testing.T) {
	key := testKey(t)
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(255 - i)
	}
	wrongKey, err := credstore.NewKey(other)
	require.NoError(t, err)

	blob, err := MarshalCredentials(ConnectorCredentials{Token: "secret"}, key)
	require.NoError(t, err)

	_, err = UnmarshalCredentials(blob, wrongKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt")
	assert.NotContains(t, err.Error(), "secret")
}

// TestUnmarshalTamperedBlobFails asserts a flipped ciphertext byte is
// caught by the GCM auth tag.
func TestUnmarshalTamperedBlobFails(t *testing.T) {
	key := testKey(t)
	blob, err := MarshalCredentials(ConnectorCredentials{Token: "secret"}, key)
	require.NoError(t, err)

	blob[len(blob)-1] ^= 0xFF
	_, err = UnmarshalCredentials(blob, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt")
}

// TestMarshalNilKey covers the nil-key guard.
func TestMarshalNilKey(t *testing.T) {
	_, err := MarshalCredentials(ConnectorCredentials{Token: "x"}, nil)
	assert.ErrorIs(t, err, errCredMarshalFailed)
}

// TestMemoryCredentialStore covers the store round-trip, Has, Delete,
// the not-found sentinel, and — crucially — that the store holds
// ciphertext at rest, not plaintext.
func TestMemoryCredentialStore(t *testing.T) {
	ctx := context.Background()
	key := testKey(t)
	store := NewMemoryCredentialStore(key)

	creds := ConnectorCredentials{Token: "super-secret-token"}

	t.Run("get missing returns not-found", func(t *testing.T) {
		_, err := store.Get(ctx, "missing")
		assert.ErrorIs(t, err, ErrCredentialsNotFound)

		has, err := store.Has(ctx, "missing")
		require.NoError(t, err)
		assert.False(t, has)
	})

	t.Run("put then get round-trips", func(t *testing.T) {
		require.NoError(t, store.Put(ctx, "conn-1", creds))

		has, err := store.Has(ctx, "conn-1")
		require.NoError(t, err)
		assert.True(t, has)

		got, err := store.Get(ctx, "conn-1")
		require.NoError(t, err)
		assert.Equal(t, creds, *got)
	})

	t.Run("stored bytes are ciphertext, not plaintext", func(t *testing.T) {
		concrete, ok := store.(*memoryCredentialStore)
		require.True(t, ok)
		concrete.mu.RLock()
		blob := concrete.sealed["conn-1"]
		concrete.mu.RUnlock()
		require.NotEmpty(t, blob)
		assert.False(t, bytes.Contains(blob, []byte("super-secret-token")), "store must hold ciphertext at rest")
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		require.NoError(t, store.Delete(ctx, "conn-1"))
		require.NoError(t, store.Delete(ctx, "conn-1")) // second delete is a no-op

		_, err := store.Get(ctx, "conn-1")
		assert.ErrorIs(t, err, ErrCredentialsNotFound)
	})
}

// TestConnectorCredentialsIsZero covers the zero-value helper.
func TestConnectorCredentialsIsZero(t *testing.T) {
	assert.True(t, ConnectorCredentials{}.IsZero())
	assert.False(t, ConnectorCredentials{Token: "x"}.IsZero())
	assert.False(t, ConnectorCredentials{Headers: map[string]string{"a": "b"}}.IsZero())
}
