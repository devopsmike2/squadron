// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSealOIDCLoginCookie_RoundTrip — ADR 0014 D7 — seal/unseal returns the
// original payload verbatim; fresh nonce per call yields distinct blobs.
func TestSealOIDCLoginCookie_RoundTrip(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte(`{"state":"s","nonce":"n","conn_id":"c","return_to":"/"}`)

	blob1, err := SealOIDCLoginCookie(key, plaintext)
	require.NoError(t, err)
	blob2, err := SealOIDCLoginCookie(key, plaintext)
	require.NoError(t, err)
	if bytes.Equal(blob1, blob2) {
		t.Fatalf("two successive seals produced the same blob — nonce reuse")
	}

	out1, err := UnsealOIDCLoginCookie(key, blob1)
	require.NoError(t, err)
	if !bytes.Equal(out1, plaintext) {
		t.Errorf("unseal blob1 = %q, want %q", out1, plaintext)
	}
	out2, err := UnsealOIDCLoginCookie(key, blob2)
	require.NoError(t, err)
	if !bytes.Equal(out2, plaintext) {
		t.Errorf("unseal blob2 = %q, want %q", out2, plaintext)
	}
}

// TestSealOIDCLoginCookie_DomainSeparator — a login-cookie blob MUST NOT open
// as an OIDC client secret (different AAD), and vice versa.
func TestSealOIDCLoginCookie_DomainSeparator(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte("shared-plaintext")

	cookieBlob, err := SealOIDCLoginCookie(key, plaintext)
	require.NoError(t, err)
	_, err = UnsealOIDCClientSecret(key, cookieBlob)
	if err == nil {
		t.Fatalf("login cookie blob unsealed as an OIDC client secret — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}

	secretBlob, err := SealOIDCClientSecret(key, plaintext)
	require.NoError(t, err)
	_, err = UnsealOIDCLoginCookie(key, secretBlob)
	if err == nil {
		t.Fatalf("OIDC client secret blob unsealed as a login cookie — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOIDCLoginCookie_NilOrEmptyInputs — defense in depth.
func TestSealOIDCLoginCookie_NilOrEmptyInputs(t *testing.T) {
	if _, err := SealOIDCLoginCookie(nil, []byte("x")); err == nil {
		t.Errorf("SealOIDCLoginCookie(nil, ...) returned no error")
	}
	key := newTestKey(t)
	if _, err := SealOIDCLoginCookie(key, nil); err == nil {
		t.Errorf("SealOIDCLoginCookie(key, nil) returned no error")
	}
	if _, err := UnsealOIDCLoginCookie(nil, []byte("x")); err == nil {
		t.Errorf("UnsealOIDCLoginCookie(nil, ...) returned no error")
	}
}
