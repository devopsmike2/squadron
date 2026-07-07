// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSealAuditCheckpoint_RoundTrip — the seal / unseal pair returns the
// original plaintext verbatim; two successive seals of the same plaintext
// produce different blobs (fresh nonce) but both open to the same plaintext.
func TestSealAuditCheckpoint_RoundTrip(t *testing.T) {
	key := newTestKey(t)
	plaintext := []byte(`{"checkpoint_seq":42,"row_hash":"abc123"}`)

	blob1, err := SealAuditCheckpoint(key, plaintext)
	require.NoError(t, err)
	blob2, err := SealAuditCheckpoint(key, plaintext)
	require.NoError(t, err)
	if bytes.Equal(blob1, blob2) {
		t.Fatalf("two successive seals produced the same blob — nonce reuse")
	}

	out1, err := UnsealAuditCheckpoint(key, blob1)
	require.NoError(t, err)
	require.True(t, bytes.Equal(out1, plaintext), "unseal blob1 mismatch")
	out2, err := UnsealAuditCheckpoint(key, blob2)
	require.NoError(t, err)
	require.True(t, bytes.Equal(out2, plaintext), "unseal blob2 mismatch")
}

// TestUnsealAuditCheckpoint_TamperDetected — flipping any ciphertext byte
// makes GCM Open reject the blob (auth-tag mismatch), surfaced as a decrypt
// failure rather than silently returning corrupted plaintext.
func TestUnsealAuditCheckpoint_TamperDetected(t *testing.T) {
	key := newTestKey(t)
	blob, err := SealAuditCheckpoint(key, []byte("checkpoint-payload"))
	require.NoError(t, err)

	// Flip a byte in the ciphertext region (past the 1-byte version + nonce).
	tampered := append([]byte(nil), blob...)
	idx := 1 + nonceByteLen
	tampered[idx] ^= 0xff

	_, err = UnsealAuditCheckpoint(key, tampered)
	require.Error(t, err, "tampered blob must not open")
	require.Contains(t, err.Error(), "decrypt", "tamper should surface as a decrypt failure")
}

// TestSealAuditCheckpoint_DomainSeparator_WebhookBlobCannotOpen — the critical
// security boundary. A blob sealed as a webhook secret MUST NOT be unsealable
// as an audit checkpoint: the distinct AAD makes GCM reject the tag.
func TestSealAuditCheckpoint_DomainSeparator_WebhookBlobCannotOpen(t *testing.T) {
	key := newTestKey(t)
	webhookBlob, err := SealWebhookSecret(key, []byte("per-connection-webhook-secret"))
	require.NoError(t, err)

	_, err = UnsealAuditCheckpoint(key, webhookBlob)
	require.Error(t, err, "webhook-sealed blob unsealed as an audit checkpoint — domain separator failed")
	require.Contains(t, err.Error(), "decrypt", "cross-AAD attempt should surface as a decrypt failure")
}

// TestUnsealAuditCheckpoint_MalformedBlob — short, empty, and unknown-version
// blobs all return ErrAuditCheckpointMalformed (schema-mismatch, not tamper).
func TestUnsealAuditCheckpoint_MalformedBlob(t *testing.T) {
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
			_, err := UnsealAuditCheckpoint(key, tc.blob)
			require.Error(t, err)
			if !errors.Is(err, ErrAuditCheckpointMalformed) {
				t.Errorf("error = %v, want ErrAuditCheckpointMalformed", err)
			}
		})
	}
}

// TestSealAuditCheckpoint_NilOrEmptyInputs — nil key is a wiring bug; empty
// plaintext is a caller bug. Both are rejected before the cipher runs.
func TestSealAuditCheckpoint_NilOrEmptyInputs(t *testing.T) {
	if _, err := SealAuditCheckpoint(nil, []byte("x")); err == nil {
		t.Errorf("SealAuditCheckpoint(nil, ...) returned no error")
	}
	key := newTestKey(t)
	if _, err := SealAuditCheckpoint(key, nil); err == nil {
		t.Errorf("SealAuditCheckpoint(key, nil) returned no error")
	}
	if _, err := UnsealAuditCheckpoint(nil, []byte("x")); err == nil {
		t.Errorf("UnsealAuditCheckpoint(nil, ...) returned no error")
	}
}
