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

// realisticSAJSON is a minimal-but-realistic GCP SA JSON shape used
// by the round-trip test. It is NOT a real credential — the private
// key is zero bytes — but its shape matches what an operator will
// paste into the wizard. v0.89.46 (#667 slice 1 chunk 1).
const realisticSAJSON = `{
  "type": "service_account",
  "project_id": "my-project",
  "private_key_id": "00000000000000000000000000000000",
  "private_key": "-----BEGIN PRIVATE KEY-----\nMIIfake\n-----END PRIVATE KEY-----\n",
  "client_email": "squadron-discovery@my-project.iam.gserviceaccount.com",
  "client_id": "000000000000000000000",
  "token_uri": "https://oauth2.googleapis.com/token"
}`

// TestSealGCPServiceAccount_RoundTrip — v0.89.46 (#667 slice 1 chunk
// 1) — the seal / unseal pair returns the original plaintext bytes
// verbatim. Two successive seals of the same plaintext produce
// different blobs (fresh nonce per call) but both open to the same
// plaintext under the same key. Pins the basic encryption contract
// for the GCP SA path; mirrors TestSealWebhookSecret_RoundTrip
// exactly.
func TestSealGCPServiceAccount_RoundTrip(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte(realisticSAJSON)

	blob1, err := SealGCPServiceAccount(key, plaintext)
	require.NoError(t, err)
	blob2, err := SealGCPServiceAccount(key, plaintext)
	require.NoError(t, err)
	if bytes.Equal(blob1, blob2) {
		t.Fatalf("two successive seals produced the same blob — nonce reuse")
	}

	out1, err := UnsealGCPServiceAccount(key, blob1)
	require.NoError(t, err)
	if !bytes.Equal(out1, plaintext) {
		t.Errorf("unseal blob1 length=%d, want length=%d (contents redacted — never log SA JSON)", len(out1), len(plaintext))
	}
	out2, err := UnsealGCPServiceAccount(key, blob2)
	require.NoError(t, err)
	if !bytes.Equal(out2, plaintext) {
		t.Errorf("unseal blob2 length=%d, want length=%d (contents redacted — never log SA JSON)", len(out2), len(plaintext))
	}

	// Envelope version byte must be 0x01 — pin the on-launch version
	// so a future bump cannot land silently.
	if blob1[0] != gcpSAEnvelopeV1 {
		t.Errorf("envelope version byte = 0x%02x, want 0x%02x", blob1[0], gcpSAEnvelopeV1)
	}
}

// TestSealGCPServiceAccount_DomainSeparation_PATBytesFail —
// v0.89.46 (#667 slice 1 chunk 1) — the critical security boundary.
// A sealed PAT (produced via the iacconnstore MarshalGitHubPATCreds
// path, which uses key.Seal with no AAD) MUST NOT be unsealable as
// a GCP SA. The AAD on the GCP SA envelope is the domain separator
// that enforces this: GCM rejects the tag because it was computed
// under a different AAD (nil vs the gcpSAAAD).
//
// We simulate the attack by sealing some plaintext with the no-AAD
// path (mimicking the PAT envelope), then handing the result to
// UnsealGCPServiceAccount. The expected outcome is a decrypt error
// — at no point should the cipher accept the cross-shape blob.
func TestSealGCPServiceAccount_DomainSeparation_PATBytesFail(t *testing.T) {
	key := newTestKey(t)
	// Build a fake "PAT-shaped" blob using the no-AAD Key.Seal path.
	// The version byte matches gcpSAEnvelopeV1 so the envelope check
	// passes — the only line of defense left is the AAD on the AEAD
	// Open.
	ciphertext, nonce, err := key.Seal([]byte("ghp_pretendThisIsAPAT"))
	require.NoError(t, err)
	patShaped := make([]byte, 0, 1+len(nonce)+len(ciphertext))
	patShaped = append(patShaped, gcpSAEnvelopeV1)
	patShaped = append(patShaped, nonce...)
	patShaped = append(patShaped, ciphertext...)

	_, err = UnsealGCPServiceAccount(key, patShaped)
	if err == nil {
		t.Fatalf("PAT blob unsealed as a GCP SA — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealGCPServiceAccount_DomainSeparation_WebhookSecretFails —
// v0.89.46 (#667 slice 1 chunk 1) — the OTHER critical security
// boundary. A sealed webhook secret (produced via SealWebhookSecret,
// AAD="squadron.webhook_secret.v1") MUST NOT be unsealable as a GCP
// SA. The two helpers share envelope version 0x01 so the envelope
// check passes; the only line of defense is the differing AAD.
func TestSealGCPServiceAccount_DomainSeparation_WebhookSecretFails(t *testing.T) {
	key := newTestKey(t)
	webhookBlob, err := SealWebhookSecret(key, []byte("hmac-secret-for-some-connection"))
	require.NoError(t, err)

	_, err = UnsealGCPServiceAccount(key, webhookBlob)
	if err == nil {
		t.Fatalf("webhook secret blob unsealed as a GCP SA — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestUnsealGCPServiceAccount_VersionMismatch_Errors — v0.89.46
// (#667 slice 1 chunk 1) — short, empty, and unknown-version blobs
// all return ErrGCPSAMalformed. These are schema-mismatch signals
// (operator restored a backup, hand-pasted the wrong value),
// distinct from decrypt failures.
func TestUnsealGCPServiceAccount_VersionMismatch_Errors(t *testing.T) {
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
			_, err := UnsealGCPServiceAccount(key, tc.blob)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, ErrGCPSAMalformed) {
				t.Errorf("error = %v, want ErrGCPSAMalformed", err)
			}
		})
	}
}

// TestSealGCPServiceAccount_TamperedCiphertext_Errors — v0.89.46
// (#667 slice 1 chunk 1) — flipping a byte in the ciphertext makes
// Open fail with an auth-tag mismatch. Pins the AES-GCM tamper
// detection on the GCP SA path.
func TestSealGCPServiceAccount_TamperedCiphertext_Errors(t *testing.T) {
	key := newTestKey(t)
	blob, err := SealGCPServiceAccount(key, []byte(realisticSAJSON))
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

	_, err = UnsealGCPServiceAccount(key, tampered)
	if err == nil {
		t.Fatalf("tampered ciphertext was unsealed — auth tag check failed to fire")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestSealGCPServiceAccount_NilOrEmptyInputs — v0.89.46 (#667 slice
// 1 chunk 1) — defense in depth around the invariants the create
// handler upstream already enforces (nil key is a wiring bug; empty
// plaintext is a caller bug — the wizard validates SA JSON shape
// before sealing).
func TestSealGCPServiceAccount_NilOrEmptyInputs(t *testing.T) {
	if _, err := SealGCPServiceAccount(nil, []byte("x")); err == nil {
		t.Errorf("SealGCPServiceAccount(nil, ...) returned no error")
	}
	key := newTestKey(t)
	if _, err := SealGCPServiceAccount(key, nil); err == nil {
		t.Errorf("SealGCPServiceAccount(key, nil) returned no error")
	}
	if _, err := UnsealGCPServiceAccount(nil, []byte("x")); err == nil {
		t.Errorf("UnsealGCPServiceAccount(nil, ...) returned no error")
	}
}
