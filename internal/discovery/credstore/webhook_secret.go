// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// webhookSecretAAD is the additional-authenticated-data tag the
// SealWebhookSecret / UnsealWebhookSecret helpers bind into the
// AES-GCM authentication tag. The string is a domain separator: a
// blob sealed under this AAD will fail to open under any other AAD
// (including the nil AAD used by Key.Seal / Key.Open for PATs and
// any future per-cred-shape AAD). v0.89.31 (#650).
//
// An attacker who somehow gains read access to the iac_connections
// table cannot swap a sealed PAT into the webhook_secret_sealed
// column and replay it as an HMAC key: the GCM tag was computed
// against a different AAD, so Open returns an auth-tag mismatch.
// The brief calls this the "critical security boundary"; the AAD
// is the mechanism.
//
// NEVER change this string after launch — sealed blobs in operator
// databases were computed against the on-launch string. A rotation
// would require a versioned envelope (which webhookSecretEnvelopeV1
// supports), not an in-place AAD edit.
const webhookSecretAAD = "squadron.webhook_secret.v1"

// webhookSecretEnvelopeV1 prefixes every sealed webhook-secret blob.
// One byte today; the value names the envelope version so a slice-N
// AAD or cipher rotation can land without ambiguously reading
// pre-rotation blobs. v0.89.31 (#650).
const webhookSecretEnvelopeV1 byte = 0x01

// ErrWebhookSecretMalformed is returned by UnsealWebhookSecret when
// the blob fails sanity checks BEFORE the cipher is asked to open it
// — wrong envelope version, too short to hold a nonce, etc. These
// signal "the blob shape isn't a v0.89.31 webhook secret" rather
// than "the cipher rejected the tag", so callers (and operators) can
// distinguish a schema-mismatch from a tamper / wrong-key event.
var ErrWebhookSecretMalformed = errors.New("credstore: webhook secret blob is malformed")

// SealWebhookSecret encrypts a per-connection GitHub webhook secret
// for at-rest storage in iac_connections.webhook_secret_sealed.
// v0.89.31 (#650).
//
// Uses the same AES-256-GCM substrate the PAT path uses — same
// SQUADRON_SECRETS_KEY, same nonce strategy, same authenticated
// encryption guarantees — but pins a distinct AAD
// (webhookSecretAAD) so a sealed PAT can NEVER be mis-unsealed as
// a webhook secret and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// The version byte is read by UnsealWebhookSecret so a future
// envelope rotation does not collide with pre-rotation blobs in
// operator databases.
//
// Returns an error if key is nil, plaintext is empty (caller bug —
// the PATCH handler validates empty-string-clears separately), or
// the cipher fails. Errors NEVER carry the plaintext bytes; the
// substrate's no-token-in-errors invariant extends to the webhook
// secret path.
func SealWebhookSecret(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealWebhookSecret: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealWebhookSecret: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: webhook secret nonce generation failed: %w", err)
	}
	// AAD-bound Seal — the AAD is verified by GCM at Open time but
	// not stored in the ciphertext. The domain separator lives in
	// the helper, not the blob.
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(webhookSecretAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, webhookSecretEnvelopeV1)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealWebhookSecret reverses SealWebhookSecret. v0.89.31 (#650).
//
// Returns an error wrapping ErrWebhookSecretMalformed when the blob
// shape doesn't match the envelope (truncated, unknown version
// byte) — these are typically schema-mismatch signals (operator
// downgraded, restored from backup, mis-pasted a value), not
// security incidents.
//
// Returns an error containing "decrypt" when the cipher rejects the
// blob — wrong SQUADRON_SECRETS_KEY (rotated), tampered ciphertext,
// or — critically — a sealed-PAT blob attempted to be unsealed
// here. The domain separator AAD makes the cross-shape attempt
// fail at the auth-tag check, indistinguishable from any other
// tamper event.
//
// On success returns the plaintext bytes (the HMAC key). Callers
// MUST NOT log these bytes or include them in error messages.
func UnsealWebhookSecret(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealWebhookSecret: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrWebhookSecretMalformed)
	}
	version := blob[0]
	if version != webhookSecretEnvelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrWebhookSecretMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(webhookSecretAAD))
	if err != nil {
		// Match the Key.Open phrasing ("decrypt failed") so callers
		// that already pattern-match on that signal for PATs see
		// the same signal here. The underlying err NEVER carries
		// plaintext (GCM tag mismatch is the only failure mode).
		return nil, fmt.Errorf("credstore: webhook secret decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
