// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// testKeyRaw returns a deterministic 32-byte key for tests. Matches
// the helper credstore's tests use so the two packages exercise the
// same key material when tested side by side.
func testKeyRaw() []byte {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1) // arbitrary, non-zero, deterministic
	}
	return raw
}

func newTestKey(t *testing.T) *credstore.Key {
	t.Helper()
	key, err := credstore.NewKey(testKeyRaw())
	require.NoError(t, err, "credstore.NewKey should succeed with 32 raw bytes")
	return key
}

func newOtherKey(t *testing.T) *credstore.Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(255 - i) // distinct from testKeyRaw
	}
	key, err := credstore.NewKey(raw)
	require.NoError(t, err)
	return key
}

// MarshalGitHubPATCreds_roundtrips_via_unmarshal: the primary
// substrate guarantee. The token roundtrips through encrypt and
// decrypt unchanged; the on-disk blob does not contain the token in
// plaintext bytes.
func TestMarshalGitHubPATCreds_roundtrips_via_unmarshal(t *testing.T) {
	key := newTestKey(t)
	creds := GitHubPATCredentials{
		Token: "ghp_exampletokenmaterialThatShouldRoundTripUnchanged",
	}

	blob, err := MarshalGitHubPATCreds(creds, key)
	require.NoError(t, err)
	require.NotEmpty(t, blob)
	require.GreaterOrEqual(t, len(blob), nonceLen,
		"blob must at least carry the 12-byte nonce prefix")

	// The token must NOT appear as plaintext bytes in the blob.
	assert.NotContains(t, string(blob), creds.Token,
		"sealed blob must not contain the plaintext token")

	got, err := UnmarshalGitHubPATCreds(blob, key)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, creds.Token, got.Token,
		"Token must roundtrip through MarshalGitHubPATCreds + UnmarshalGitHubPATCreds")
}

// UnmarshalGitHubPATCreds_with_wrong_key_returns_decrypt_error_with_no_token_bytes:
// the substrate's tamper / wrong-key path must surface a stable
// "decrypt"-bearing error and MUST NOT carry token bytes (or any
// plaintext fragment) in the error message.
func TestUnmarshalGitHubPATCreds_with_wrong_key_returns_decrypt_error_with_no_token_bytes(t *testing.T) {
	rightKey := newTestKey(t)
	wrongKey := newOtherKey(t)

	creds := GitHubPATCredentials{
		Token: "ghp_secretTokenThatMustNotLeakIntoAnyErrorMessage",
	}
	blob, err := MarshalGitHubPATCreds(creds, rightKey)
	require.NoError(t, err)

	_, err = UnmarshalGitHubPATCreds(blob, wrongKey)
	require.Error(t, err, "decrypt with the wrong key must fail")

	msg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(msg, "decrypt") || strings.Contains(msg, "auth"),
		"wrong-key error should contain 'decrypt' or 'auth'; got %q", err.Error())

	// The error must NOT carry the token (or any plausible fragment
	// of it) under any circumstance. We check for the prefix
	// because token strings get truncated in some error formats.
	assert.NotContains(t, err.Error(), creds.Token,
		"error must NOT contain the plaintext token")
	assert.NotContains(t, err.Error(), "ghp_",
		"error must NOT contain even a token-prefix substring")

	// Also exercise the truncated-blob path: a blob shorter than
	// the nonce can't even be opened; the same no-leak guarantee
	// must hold.
	_, err = UnmarshalGitHubPATCreds([]byte{0x01, 0x02, 0x03}, rightKey)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "decrypt",
		"too-short blob must surface a decrypt error")
}

// MarshalGitHubPATCreds_with_empty_token_returns_validation_error:
// the marshal helper enforces the "Token must not be empty" rule
// from the wizard so a programming error doesn't silently produce a
// connection with an unusable credential blob.
func TestMarshalGitHubPATCreds_with_empty_token_returns_validation_error(t *testing.T) {
	key := newTestKey(t)
	blob, err := MarshalGitHubPATCreds(GitHubPATCredentials{Token: ""}, key)
	require.Error(t, err)
	require.Nil(t, blob, "no blob must be returned when validation fails")

	// The error must be the typed cred-marshal-failed sentinel so
	// handlers can errors.Is against it.
	assert.ErrorIs(t, err, errCredMarshalFailed)

	// And the error string must name the offending field so
	// operators get an actionable signal.
	assert.Contains(t, err.Error(), "Token is required")
}

// TestMarshalGitHubPATCreds_with_nil_key_rejected: the key is a
// required input. Passing nil is a programmer error.
func TestMarshalGitHubPATCreds_with_nil_key_rejected(t *testing.T) {
	_, err := MarshalGitHubPATCreds(GitHubPATCredentials{Token: "ghp_x"}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCredMarshalFailed)
	assert.Contains(t, err.Error(), "key is required")
}

// TestMarshalGitHubPATCreds_tamper_detection: flipping one byte in
// the ciphertext part of the blob must cause Unmarshal to fail with
// a decrypt error. Parallel to credstore's tamper test for
// AWSCredentials.
func TestMarshalGitHubPATCreds_tamper_detection(t *testing.T) {
	key := newTestKey(t)
	creds := GitHubPATCredentials{Token: "ghp_tampertest"}

	blob, err := MarshalGitHubPATCreds(creds, key)
	require.NoError(t, err)
	require.Greater(t, len(blob), nonceLen,
		"blob must have at least one ciphertext byte to tamper with")

	tampered := make([]byte, len(blob))
	copy(tampered, blob)
	// Flip a byte in the ciphertext, not the nonce — both paths fail
	// the auth tag, but flipping ciphertext is the more interesting
	// test of GCM's integrity guarantee.
	tampered[nonceLen] ^= 0xFF

	_, err = UnmarshalGitHubPATCreds(tampered, key)
	require.Error(t, err)
	msg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(msg, "decrypt") || strings.Contains(msg, "auth"),
		"tamper error should contain 'decrypt' or 'auth'; got %q", err.Error())
	assert.NotContains(t, err.Error(), creds.Token,
		"tamper error must NOT contain the plaintext token")
}
