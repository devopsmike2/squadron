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

// TestSealWebhookSecret_RoundTrip — v0.89.31 (#650) — the seal /
// unseal pair returns the original plaintext bytes verbatim. Two
// successive seals of the same plaintext produce different blobs
// (fresh nonce per call) but both open to the same plaintext under
// the same key. Pins the basic encryption contract.
func TestSealWebhookSecret_RoundTrip(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte("per-connection-webhook-secret-abc123")

	blob1, err := SealWebhookSecret(key, plaintext)
	require.NoError(t, err)
	blob2, err := SealWebhookSecret(key, plaintext)
	require.NoError(t, err)
	if bytes.Equal(blob1, blob2) {
		t.Fatalf("two successive seals produced the same blob — nonce reuse")
	}

	out1, err := UnsealWebhookSecret(key, blob1)
	require.NoError(t, err)
	if !bytes.Equal(out1, plaintext) {
		t.Errorf("unseal blob1 = %q, want %q", out1, plaintext)
	}
	out2, err := UnsealWebhookSecret(key, blob2)
	require.NoError(t, err)
	if !bytes.Equal(out2, plaintext) {
		t.Errorf("unseal blob2 = %q, want %q", out2, plaintext)
	}
}

// TestSealWebhookSecret_DomainSeparator_PATBlobCannotOpenAsWebhookSecret
// — v0.89.31 (#650) — the critical security boundary. A sealed PAT
// (produced via the iacconnstore.MarshalGitHubPATCreds path, which
// uses key.Seal with no AAD) MUST NOT be unsealable as a webhook
// secret. The AAD on the webhook-secret envelope is the domain
// separator that enforces this: GCM rejects the tag because it was
// computed under a different AAD (nil vs the webhook AAD).
//
// We simulate the attack here by sealing some plaintext with the
// no-AAD path (mimicking the PAT envelope), then handing the result
// to UnsealWebhookSecret. The expected outcome is a decrypt error
// — at no point should the cipher accept the cross-shape blob.
func TestSealWebhookSecret_DomainSeparator_PATBlobCannotOpenAsWebhookSecret(t *testing.T) {
	key := newTestKey(t)
	// Build a fake "PAT-shaped" blob by hand: [1-byte version][12-byte nonce][ct].
	// The version byte matches webhookSecretEnvelopeV1 so the
	// envelope check passes — the only line of defense left is the
	// AAD on the AEAD Open.
	ciphertext, nonce, err := key.Seal([]byte("ghp_pretendThisIsAPAT"))
	require.NoError(t, err)
	patShaped := make([]byte, 0, 1+len(nonce)+len(ciphertext))
	patShaped = append(patShaped, webhookSecretEnvelopeV1)
	patShaped = append(patShaped, nonce...)
	patShaped = append(patShaped, ciphertext...)

	_, err = UnsealWebhookSecret(key, patShaped)
	if err == nil {
		t.Fatalf("PAT blob unsealed as a webhook secret — domain separator failed")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("error = %v; want it to surface as a decrypt failure", err)
	}
}

// TestUnsealWebhookSecret_MalformedBlob — short, empty, and unknown-
// version blobs all return ErrWebhookSecretMalformed. These are
// schema-mismatch signals (operator restored a backup, hand-pasted
// the wrong value), distinct from decrypt failures.
func TestUnsealWebhookSecret_MalformedBlob(t *testing.T) {
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
			_, err := UnsealWebhookSecret(key, tc.blob)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, ErrWebhookSecretMalformed) {
				t.Errorf("error = %v, want ErrWebhookSecretMalformed", err)
			}
		})
	}
}

// TestSealWebhookSecret_NilOrEmptyInputs — defense in depth around
// the invariants the PATCH handler upstream already enforces (nil
// key is a wiring bug; empty plaintext is the "clear the column"
// path that bypasses the seal call entirely).
func TestSealWebhookSecret_NilOrEmptyInputs(t *testing.T) {
	if _, err := SealWebhookSecret(nil, []byte("x")); err == nil {
		t.Errorf("SealWebhookSecret(nil, ...) returned no error")
	}
	key := newTestKey(t)
	if _, err := SealWebhookSecret(key, nil); err == nil {
		t.Errorf("SealWebhookSecret(key, nil) returned no error")
	}
	if _, err := UnsealWebhookSecret(nil, []byte("x")); err == nil {
		t.Errorf("UnsealWebhookSecret(nil, ...) returned no error")
	}
}
