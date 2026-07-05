// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSealOIDCClientSecret_RoundTrip — ADR 0014 D5 — seal/unseal returns the
// original plaintext verbatim; two successive seals of the same plaintext
// produce different blobs (fresh nonce) but both open to the same plaintext.
func TestSealOIDCClientSecret_RoundTrip(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte("oidc-client-secret-xyz789")

	blob1, err := SealOIDCClientSecret(key, plaintext)
	require.NoError(t, err)
	blob2, err := SealOIDCClientSecret(key, plaintext)
	require.NoError(t, err)
	if bytes.Equal(blob1, blob2) {
		t.Fatalf("two successive seals produced the same blob — nonce reuse")
	}

	out1, err := UnsealOIDCClientSecret(key, blob1)
	require.NoError(t, err)
	if !bytes.Equal(out1, plaintext) {
		t.Errorf("unseal blob1 = %q, want %q", out1, plaintext)
	}
	out2, err := UnsealOIDCClientSecret(key, blob2)
	require.NoError(t, err)
	if !bytes.Equal(out2, plaintext) {
		t.Errorf("unseal blob2 = %q, want %q", out2, plaintext)
	}
}

// TestSealOIDCClientSecret_DomainSeparator — the critical AAD boundary. A blob
// sealed as an OIDC client secret MUST NOT open as a webhook secret (different
// AAD), and vice versa. The GCM tag was computed under a different AAD, so Open
// rejects it as a decrypt failure.
func TestSealOIDCClientSecret_DomainSeparator(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte("shared-plaintext")

	oidcBlob, err := SealOIDCClientSecret(key, plaintext)
	require.NoError(t, err)
	// Seal as OIDC, unseal as webhook → must fail.
	_, err = UnsealWebhookSecret(key, oidcBlob)
	if err == nil {
		t.Fatalf("OIDC client secret blob unsealed as a webhook secret — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}

	// And the reverse: seal as webhook, unseal as OIDC → must fail.
	webhookBlob, err := SealWebhookSecret(key, plaintext)
	require.NoError(t, err)
	_, err = UnsealOIDCClientSecret(key, webhookBlob)
	if err == nil {
		t.Fatalf("webhook secret blob unsealed as an OIDC client secret — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOIDCClientSecret_NilOrEmptyInputs — defense in depth around the
// wiring invariants (nil key is a wiring bug; empty plaintext is a caller bug).
func TestSealOIDCClientSecret_NilOrEmptyInputs(t *testing.T) {
	if _, err := SealOIDCClientSecret(nil, []byte("x")); err == nil {
		t.Errorf("SealOIDCClientSecret(nil, ...) returned no error")
	}
	key := newTestKey(t)
	if _, err := SealOIDCClientSecret(key, nil); err == nil {
		t.Errorf("SealOIDCClientSecret(key, nil) returned no error")
	}
	if _, err := UnsealOIDCClientSecret(nil, []byte("x")); err == nil {
		t.Errorf("UnsealOIDCClientSecret(nil, ...) returned no error")
	}
}
