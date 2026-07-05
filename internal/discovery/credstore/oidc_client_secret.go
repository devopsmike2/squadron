// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// oidcClientSecretAAD is the additional-authenticated-data tag the
// SealOIDCClientSecret / UnsealOIDCClientSecret helpers bind into the
// AES-GCM authentication tag. The string is a domain separator: a
// blob sealed under this AAD will fail to open under any other AAD
// (the nil AAD used by Key.Seal / Key.Open for PATs, the webhook
// secret AAD, and any future per-cred-shape AAD). ADR 0014 D5.
//
// An attacker who somehow gains read access to the oidc_connections
// table cannot swap a sealed PAT or webhook secret into the
// client_secret_sealed column and replay it: the GCM tag was
// computed against a different AAD, so Open returns an auth-tag
// mismatch.
//
// NEVER change this string after launch — sealed blobs in operator
// databases were computed against the on-launch string. A rotation
// would require a versioned envelope (which oidcClientSecretEnvelopeV1
// supports), not an in-place AAD edit.
const oidcClientSecretAAD = "squadron.oidc_client_secret.v1"

// oidcClientSecretEnvelopeV1 prefixes every sealed OIDC-client-secret
// blob. One byte today; the value names the envelope version so a
// later AAD or cipher rotation can land without ambiguously reading
// pre-rotation blobs. ADR 0014 D5.
const oidcClientSecretEnvelopeV1 byte = 0x01

// ErrOIDCClientSecretMalformed is returned by UnsealOIDCClientSecret
// when the blob fails sanity checks BEFORE the cipher is asked to
// open it — wrong envelope version, too short to hold a nonce, etc.
// These signal "the blob shape isn't an OIDC client secret" rather
// than "the cipher rejected the tag", so callers (and operators) can
// distinguish a schema-mismatch from a tamper / wrong-key event.
var ErrOIDCClientSecretMalformed = errors.New("credstore: oidc client secret blob is malformed")

// SealOIDCClientSecret encrypts a per-connection OIDC client secret
// for at-rest storage in oidc_connections.client_secret_sealed.
// ADR 0014 D5.
//
// Uses the same AES-256-GCM substrate the PAT/webhook paths use —
// same SQUADRON_SECRETS_KEY, same nonce strategy, same authenticated
// encryption guarantees — but pins a distinct AAD
// (oidcClientSecretAAD) so a sealed PAT or webhook secret can NEVER
// be mis-unsealed as an OIDC client secret and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// The version byte is read by UnsealOIDCClientSecret so a future
// envelope rotation does not collide with pre-rotation blobs in
// operator databases.
//
// Returns an error if key is nil, plaintext is empty, or the cipher
// fails. Errors NEVER carry the plaintext bytes.
func SealOIDCClientSecret(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealOIDCClientSecret: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealOIDCClientSecret: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: oidc client secret nonce generation failed: %w", err)
	}
	// AAD-bound Seal — the AAD is verified by GCM at Open time but
	// not stored in the ciphertext. The domain separator lives in
	// the helper, not the blob.
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(oidcClientSecretAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, oidcClientSecretEnvelopeV1)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealOIDCClientSecret reverses SealOIDCClientSecret. ADR 0014 D5.
//
// Returns an error wrapping ErrOIDCClientSecretMalformed when the
// blob shape doesn't match the envelope (truncated, unknown version
// byte) — schema-mismatch signals, not security incidents.
//
// Returns an error containing "decrypt" when the cipher rejects the
// blob — wrong SQUADRON_SECRETS_KEY (rotated), tampered ciphertext,
// or — critically — a sealed-PAT / webhook-secret blob attempted to
// be unsealed here. The domain separator AAD makes the cross-shape
// attempt fail at the auth-tag check.
//
// On success returns the plaintext bytes. Callers MUST NOT log these
// bytes or include them in error messages.
func UnsealOIDCClientSecret(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealOIDCClientSecret: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrOIDCClientSecretMalformed)
	}
	version := blob[0]
	if version != oidcClientSecretEnvelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrOIDCClientSecretMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(oidcClientSecretAAD))
	if err != nil {
		return nil, fmt.Errorf("credstore: oidc client secret decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
