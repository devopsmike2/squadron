// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// auditCheckpointAAD is the additional-authenticated-data tag the
// SealAuditCheckpoint / UnsealAuditCheckpoint helpers bind into the
// AES-GCM authentication tag (ADR 0027 slice 2). Like webhookSecretAAD it
// is a domain separator: a blob sealed under this AAD fails to open under
// any other AAD (nil, the webhook-secret AAD, or any future per-shape AAD).
//
// Enterprise slice 3 seals retention checkpoints (the pruned head) so an
// attestation surface can prove a checkpoint was recorded by the operator's
// key and not forged. The distinct AAD stops a webhook-secret or PAT blob
// from ever being replayed as a sealed checkpoint and vice versa.
//
// NEVER change this string after launch — sealed blobs were computed against
// it. A rotation requires a versioned envelope (auditCheckpointEnvelopeV1
// supports one), not an in-place AAD edit.
const auditCheckpointAAD = "squadron.audit_checkpoint.v1"

// auditCheckpointEnvelopeV1 prefixes every sealed audit-checkpoint blob. One
// byte today; the value names the envelope version so a later AAD or cipher
// rotation can land without ambiguously reading pre-rotation blobs.
const auditCheckpointEnvelopeV1 byte = 0x01

// ErrAuditCheckpointMalformed is returned by UnsealAuditCheckpoint when the
// blob fails sanity checks BEFORE the cipher is asked to open it — wrong
// envelope version, too short to hold a nonce, etc. These signal "the blob
// shape isn't a v1 audit checkpoint" rather than "the cipher rejected the
// tag", so callers can distinguish a schema-mismatch from a tamper event.
var ErrAuditCheckpointMalformed = errors.New("credstore: audit checkpoint blob is malformed")

// SealAuditCheckpoint encrypts a retention/chain checkpoint payload for
// at-rest storage / attestation (ADR 0027 slice 2). It uses the same
// AES-256-GCM substrate as the PAT and webhook-secret paths but pins a
// distinct AAD (auditCheckpointAAD) so a checkpoint blob can never be
// mis-unsealed as another cred shape and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// Returns an error if key is nil, plaintext is empty, or the cipher fails.
// Errors NEVER carry the plaintext bytes.
func SealAuditCheckpoint(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealAuditCheckpoint: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealAuditCheckpoint: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: audit checkpoint nonce generation failed: %w", err)
	}
	// AAD-bound Seal — the AAD is verified by GCM at Open time but not stored
	// in the ciphertext. The domain separator lives in the helper, not the blob.
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(auditCheckpointAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, auditCheckpointEnvelopeV1)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealAuditCheckpoint reverses SealAuditCheckpoint (ADR 0027 slice 2).
//
// Returns an error wrapping ErrAuditCheckpointMalformed when the blob shape
// doesn't match the envelope (truncated, unknown version byte). Returns an
// error containing "decrypt" when the cipher rejects the blob — wrong key,
// tampered ciphertext, or a cross-shape blob (webhook secret / PAT) whose AAD
// no longer matches. On success returns the plaintext bytes; callers MUST NOT
// log them or include them in error messages.
func UnsealAuditCheckpoint(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealAuditCheckpoint: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrAuditCheckpointMalformed)
	}
	version := blob[0]
	if version != auditCheckpointEnvelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrAuditCheckpointMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(auditCheckpointAAD))
	if err != nil {
		return nil, fmt.Errorf("credstore: audit checkpoint decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
