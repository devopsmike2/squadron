// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// gcpSAAAD is the additional-authenticated-data tag the
// SealGCPServiceAccount / UnsealGCPServiceAccount helpers bind into
// the AES-GCM authentication tag. The string is a domain separator: a
// blob sealed under this AAD will fail to open under any other AAD
// (including the nil AAD used by Key.Seal / Key.Open for PATs and the
// webhookSecretAAD used for per-connection webhook secrets). v0.89.46
// (#667 slice 1 chunk 1).
//
// An attacker who somehow gains read access to the gcp_connections
// table cannot swap a sealed PAT into the sealed_sa column and replay
// it as a Service Account JSON: the GCM tag was computed against a
// different AAD, so Open returns an auth-tag mismatch. The brief
// names this the critical domain-separation security boundary
// (parallel to v0.89.31's webhook-secret posture); the AAD is the
// mechanism.
//
// NEVER change this string after launch — sealed blobs in operator
// databases were computed against the on-launch string. A rotation
// would require a versioned envelope (which gcpSAEnvelopeV1
// supports), not an in-place AAD edit.
const gcpSAAAD = "squadron.gcp_sa.v1"

// gcpSAEnvelopeV1 prefixes every sealed GCP-SA-JSON blob. One byte
// today; the value names the envelope version so a slice-N AAD or
// cipher rotation can land without ambiguously reading pre-rotation
// blobs. v0.89.46 (#667 slice 1 chunk 1).
const gcpSAEnvelopeV1 byte = 0x01

// ErrGCPSAMalformed is returned by UnsealGCPServiceAccount when the
// blob fails sanity checks BEFORE the cipher is asked to open it —
// wrong envelope version, too short to hold a nonce, etc. These
// signal "the blob shape isn't a v0.89.46 GCP SA" rather than "the
// cipher rejected the tag", so callers (and operators) can
// distinguish a schema-mismatch from a tamper / wrong-key event.
var ErrGCPSAMalformed = errors.New("credstore: GCP SA blob is malformed")

// SealGCPServiceAccount encrypts a Service Account JSON key for
// at-rest storage in gcp_connections.sealed_sa. v0.89.46 (#667 slice
// 1 chunk 1).
//
// Uses the same AES-256-GCM substrate the PAT and webhook-secret
// paths use — same SQUADRON_SECRETS_KEY, same nonce strategy, same
// authenticated encryption guarantees — but pins a distinct AAD
// (gcpSAAAD) so a sealed PAT or webhook secret can NEVER be
// mis-unsealed as a GCP SA and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// The version byte is read by UnsealGCPServiceAccount so a future
// envelope rotation does not collide with pre-rotation blobs in
// operator databases.
//
// Returns an error if key is nil, plaintext is empty (caller bug —
// the create handler validates empty-SA-JSON separately), or the
// cipher fails. Errors NEVER carry the plaintext bytes; the
// substrate's no-credential-in-errors invariant extends to the GCP
// SA path. The plaintext SA JSON is NEVER logged, NEVER embedded in
// audit payloads, and NEVER returned in HTTP responses — the seal /
// unseal pair is the only sanctioned access path.
func SealGCPServiceAccount(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealGCPServiceAccount: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealGCPServiceAccount: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: GCP SA nonce generation failed: %w", err)
	}
	// AAD-bound Seal — the AAD is verified by GCM at Open time but
	// not stored in the ciphertext. The domain separator lives in
	// the helper, not the blob.
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(gcpSAAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, gcpSAEnvelopeV1)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealGCPServiceAccount reverses SealGCPServiceAccount. v0.89.46
// (#667 slice 1 chunk 1).
//
// Returns an error wrapping ErrGCPSAMalformed when the blob shape
// doesn't match the envelope (truncated, unknown version byte) —
// these are typically schema-mismatch signals (operator downgraded,
// restored from backup, mis-pasted a value), not security incidents.
//
// Returns an error containing "decrypt" when the cipher rejects the
// blob — wrong SQUADRON_SECRETS_KEY (rotated), tampered ciphertext,
// or — critically — a sealed-PAT or sealed-webhook-secret blob
// attempted to be unsealed here. The domain separator AAD makes
// every cross-shape attempt fail at the auth-tag check,
// indistinguishable from any other tamper event.
//
// On success returns the plaintext SA JSON bytes. Callers MUST NOT
// log these bytes, include them in error messages, embed them in
// audit payloads, or return them in any HTTP response. The only
// sanctioned consumer is the scanner instantiating a google-cloud-go
// client.
func UnsealGCPServiceAccount(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealGCPServiceAccount: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrGCPSAMalformed)
	}
	version := blob[0]
	if version != gcpSAEnvelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrGCPSAMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(gcpSAAAD))
	if err != nil {
		// Match the Key.Open and UnsealWebhookSecret phrasing
		// ("decrypt failed") so callers that already pattern-match on
		// that signal for PATs / webhook secrets see the same signal
		// here. The underlying err NEVER carries plaintext (GCM tag
		// mismatch is the only failure mode).
		return nil, fmt.Errorf("credstore: GCP SA decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
