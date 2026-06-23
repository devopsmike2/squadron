// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// realisticOCIPrivateKey is a minimal-but-realistic OCI API Signing
// Key private key used by the round-trip test. It is NOT a real
// credential — the inner key material is fake — but its shape (a
// PEM-encoded RSA private key) matches what an operator will paste
// from `oci setup keys` / `openssl genrsa` output. v0.89.56 (#681
// slice 1 chunk 1).
const realisticOCIPrivateKey = `-----BEGIN PRIVATE KEY-----
MIIfakeNotARealKeyButShapedLikeAPEMEncodedRSAPrivateKeyBlock
abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456
-----END PRIVATE KEY-----
`

// TestSealOCIPrivateKey_RoundTrip — v0.89.56 (#681 slice 1 chunk 1)
// — the seal / unseal pair returns the original plaintext bytes
// verbatim. Two successive seals of the same plaintext produce
// different blobs (fresh nonce per call) but both open to the same
// plaintext under the same key. Pins the basic encryption contract
// for the OCI signing key path; mirrors
// TestSealAzureClientSecret_RoundTrip and
// TestSealGCPServiceAccount_RoundTrip exactly.
func TestSealOCIPrivateKey_RoundTrip(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte(realisticOCIPrivateKey)

	blob1, err := SealOCIPrivateKey(key, plaintext)
	require.NoError(t, err)
	blob2, err := SealOCIPrivateKey(key, plaintext)
	require.NoError(t, err)
	if bytes.Equal(blob1, blob2) {
		t.Fatalf("two successive seals produced the same blob — nonce reuse")
	}

	out1, err := UnsealOCIPrivateKey(key, blob1)
	require.NoError(t, err)
	if !bytes.Equal(out1, plaintext) {
		t.Errorf("unseal blob1 length=%d, want length=%d (contents redacted — never log private key)", len(out1), len(plaintext))
	}
	out2, err := UnsealOCIPrivateKey(key, blob2)
	require.NoError(t, err)
	if !bytes.Equal(out2, plaintext) {
		t.Errorf("unseal blob2 length=%d, want length=%d (contents redacted — never log private key)", len(out2), len(plaintext))
	}

	// Envelope version byte must be 0x01 — pin the on-launch
	// version so a future bump cannot land silently.
	if blob1[0] != ociSigningKeyVersion {
		t.Errorf("envelope version byte = 0x%02x, want 0x%02x", blob1[0], ociSigningKeyVersion)
	}
}

// TestSealOCIPrivateKey_DomainSeparation_PATBytesFail — v0.89.56
// (#681 slice 1 chunk 1) — the critical security boundary. A sealed
// PAT (produced via the iacconnstore MarshalGitHubPATCreds path,
// which uses key.Seal with no AAD) MUST NOT be unsealable as an OCI
// signing key. The AAD on the OCI signing key envelope is the
// domain separator that enforces this: GCM rejects the tag because
// it was computed under a different AAD (nil vs ociSigningKeyAAD).
//
// We simulate the attack by sealing some plaintext with the no-AAD
// path (mimicking the PAT envelope), then handing the result to
// UnsealOCIPrivateKey. The expected outcome is a decrypt error —
// at no point should the cipher accept the cross-shape blob.
func TestSealOCIPrivateKey_DomainSeparation_PATBytesFail(t *testing.T) {
	key := newTestKey(t)
	// Build a fake "PAT-shaped" blob using the no-AAD Key.Seal path.
	// The version byte matches ociSigningKeyVersion so the envelope
	// check passes — the only line of defense left is the AAD on the
	// AEAD Open.
	ciphertext, nonce, err := key.Seal([]byte("ghp_pretendThisIsAPAT"))
	require.NoError(t, err)
	patShaped := make([]byte, 0, 1+len(nonce)+len(ciphertext))
	patShaped = append(patShaped, ociSigningKeyVersion)
	patShaped = append(patShaped, nonce...)
	patShaped = append(patShaped, ciphertext...)

	_, err = UnsealOCIPrivateKey(key, patShaped)
	if err == nil {
		t.Fatalf("PAT blob unsealed as an OCI signing key — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOCIPrivateKey_DomainSeparation_WebhookSecretFails —
// v0.89.56 (#681 slice 1 chunk 1) — the SECOND critical security
// boundary. A sealed webhook secret (produced via SealWebhookSecret,
// AAD="squadron.webhook_secret.v1") MUST NOT be unsealable as an OCI
// signing key. The two helpers share envelope version 0x01 so the
// envelope check passes; the only line of defense is the differing
// AAD.
func TestSealOCIPrivateKey_DomainSeparation_WebhookSecretFails(t *testing.T) {
	key := newTestKey(t)
	webhookBlob, err := SealWebhookSecret(key, []byte("hmac-secret-for-some-connection"))
	require.NoError(t, err)

	_, err = UnsealOCIPrivateKey(key, webhookBlob)
	if err == nil {
		t.Fatalf("webhook secret blob unsealed as an OCI signing key — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOCIPrivateKey_DomainSeparation_GCPSAFails — v0.89.56
// (#681 slice 1 chunk 1) — the THIRD critical security boundary. A
// sealed GCP SA JSON (produced via SealGCPServiceAccount,
// AAD="squadron.gcp_sa.v1") MUST NOT be unsealable as an OCI
// signing key. The two helpers share envelope version 0x01 so the
// envelope check passes; the only line of defense is the differing
// AAD.
func TestSealOCIPrivateKey_DomainSeparation_GCPSAFails(t *testing.T) {
	key := newTestKey(t)
	gcpBlob, err := SealGCPServiceAccount(key, []byte(realisticSAJSON))
	require.NoError(t, err)

	_, err = UnsealOCIPrivateKey(key, gcpBlob)
	if err == nil {
		t.Fatalf("GCP SA blob unsealed as an OCI signing key — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOCIPrivateKey_DomainSeparation_AzureClientSecretFails —
// v0.89.56 (#681 slice 1 chunk 1) — the FOURTH critical security
// boundary, new to the OCI path because OCI is the fourth sealed
// credential type. A sealed Azure SP client_secret (produced via
// SealAzureClientSecret, AAD="squadron.azure_client_secret.v1") MUST
// NOT be unsealable as an OCI signing key. The two helpers share
// envelope version 0x01 so the envelope check passes; the only line
// of defense is the differing AAD. Defense-in-depth across the full
// cross-product of sealed credential types: PAT, webhook secret,
// GCP SA, Azure SP client_secret, AND OCI signing key.
func TestSealOCIPrivateKey_DomainSeparation_AzureClientSecretFails(t *testing.T) {
	key := newTestKey(t)
	azureBlob, err := SealAzureClientSecret(key, []byte(realisticAzureClientSecret))
	require.NoError(t, err)

	_, err = UnsealOCIPrivateKey(key, azureBlob)
	if err == nil {
		t.Fatalf("Azure SP client_secret blob unsealed as an OCI signing key — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOCIPrivateKey_VersionMismatch_Errors — v0.89.56 (#681
// slice 1 chunk 1) — short, empty, and unknown-version blobs all
// return ErrOCISigningKeyMalformed. These are schema-mismatch
// signals (operator restored a backup, hand-pasted the wrong
// value), distinct from decrypt failures.
func TestSealOCIPrivateKey_VersionMismatch_Errors(t *testing.T) {
	key := newTestKey(t)
	cases := []struct {
		name string
		blob []byte
	}{
		{"empty", nil},
		{"too short for envelope header", []byte{0x01, 0x02}},
		{"unknown envelope version", append([]byte{0xff}, bytes.Repeat([]byte{0x00}, nonceByteLen+16)...)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnsealOCIPrivateKey(key, tc.blob)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, ErrOCISigningKeyMalformed) {
				t.Errorf("error = %v, want ErrOCISigningKeyMalformed", err)
			}
		})
	}
}

// TestSealOCIPrivateKey_TamperedCiphertext_Errors — v0.89.56 (#681
// slice 1 chunk 1) — flipping a byte in the ciphertext makes Open
// fail with an auth-tag mismatch. Pins the AES-GCM tamper detection
// on the OCI signing key path.
func TestSealOCIPrivateKey_TamperedCiphertext_Errors(t *testing.T) {
	key := newTestKey(t)
	blob, err := SealOCIPrivateKey(key, []byte(realisticOCIPrivateKey))
	require.NoError(t, err)

	// Flip a byte deep in the ciphertext (well past the version +
	// nonce header). Any single-bit change anywhere in the
	// ciphertext or auth tag is sufficient.
	tampered := append([]byte(nil), blob...)
	idx := 1 + nonceByteLen + 4 // 4 bytes into the ciphertext
	if idx >= len(tampered) {
		t.Fatalf("sealed blob unexpectedly short: %d bytes", len(tampered))
	}
	tampered[idx] ^= 0x01

	_, err = UnsealOCIPrivateKey(key, tampered)
	if err == nil {
		t.Fatalf("tampered ciphertext was unsealed — auth tag check failed to fire")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealOCIPrivateKey_NilOrEmptyInputs — v0.89.56 (#681 slice 1
// chunk 1) — defense in depth around the invariants the create
// handler upstream already enforces (nil key is a wiring bug; empty
// plaintext is a caller bug — the wizard validates private key
// shape before sealing).
func TestSealOCIPrivateKey_NilOrEmptyInputs(t *testing.T) {
	if _, err := SealOCIPrivateKey(nil, []byte("x")); err == nil {
		t.Errorf("SealOCIPrivateKey(nil, ...) returned no error")
	}
	key := newTestKey(t)
	if _, err := SealOCIPrivateKey(key, nil); err == nil {
		t.Errorf("SealOCIPrivateKey(key, nil) returned no error")
	}
	if _, err := UnsealOCIPrivateKey(nil, []byte("x")); err == nil {
		t.Errorf("UnsealOCIPrivateKey(nil, ...) returned no error")
	}
}
