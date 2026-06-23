// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// azureSPAAD is the additional-authenticated-data tag the
// SealAzureClientSecret / UnsealAzureClientSecret helpers bind into
// the AES-GCM authentication tag. The string is a domain separator: a
// blob sealed under this AAD will fail to open under any other AAD
// (including the nil AAD used by Key.Seal / Key.Open for PATs, the
// webhookSecretAAD used for per-connection webhook secrets, and the
// gcpSAAAD used for GCP Service Account JSON). v0.89.51 (#674 slice 1
// chunk 1).
//
// An attacker who somehow gains read access to the azure_connections
// table cannot swap a sealed PAT, webhook secret, or GCP SA JSON into
// the sealed_secret column and replay it as an Azure SP client_secret:
// the GCM tag was computed against a different AAD, so Open returns
// an auth-tag mismatch. The brief names this the critical
// domain-separation security boundary (parallel to v0.89.31's
// webhook-secret posture and v0.89.46's GCP SA posture); the AAD is
// the mechanism. This is the third sealed credential type — defense
// in depth applies across PAT, webhook secret, GCP SA, AND Azure SP
// client_secret.
//
// NEVER change this string after launch — sealed blobs in operator
// databases were computed against the on-launch string. A rotation
// would require a versioned envelope (which azureSPEnvelopeV1
// supports), not an in-place AAD edit.
const azureSPAAD = "squadron.azure_client_secret.v1"

// azureSPEnvelopeV1 prefixes every sealed Azure SP client_secret blob.
// One byte today; the value names the envelope version so a slice-N
// AAD or cipher rotation can land without ambiguously reading
// pre-rotation blobs. v0.89.51 (#674 slice 1 chunk 1).
const azureSPEnvelopeV1 byte = 0x01

// ErrAzureSPMalformed is returned by UnsealAzureClientSecret when the
// blob fails sanity checks BEFORE the cipher is asked to open it —
// wrong envelope version, too short to hold a nonce, etc. These
// signal "the blob shape isn't a v0.89.51 Azure SP client_secret"
// rather than "the cipher rejected the tag", so callers (and
// operators) can distinguish a schema-mismatch from a tamper /
// wrong-key event.
var ErrAzureSPMalformed = errors.New("credstore: Azure SP client_secret blob is malformed")

// SealAzureClientSecret encrypts a Service Principal client_secret
// for at-rest storage in azure_connections.sealed_secret. v0.89.51
// (#674 slice 1 chunk 1).
//
// Uses the same AES-256-GCM substrate the PAT, webhook-secret, and
// GCP SA paths use — same SQUADRON_SECRETS_KEY, same nonce strategy,
// same authenticated encryption guarantees — but pins a distinct AAD
// (azureSPAAD) so a sealed PAT, webhook secret, or GCP SA JSON can
// NEVER be mis-unsealed as an Azure SP client_secret and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// The version byte is read by UnsealAzureClientSecret so a future
// envelope rotation does not collide with pre-rotation blobs in
// operator databases.
//
// Returns an error if key is nil, plaintext is empty (caller bug —
// the create handler validates empty-secret separately), or the
// cipher fails. Errors NEVER carry the plaintext bytes; the
// substrate's no-credential-in-errors invariant extends to the Azure
// SP path. The plaintext client_secret is NEVER logged, NEVER
// embedded in audit payloads, and NEVER returned in HTTP responses —
// the seal / unseal pair is the only sanctioned access path.
func SealAzureClientSecret(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealAzureClientSecret: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealAzureClientSecret: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: Azure SP client_secret nonce generation failed: %w", err)
	}
	// AAD-bound Seal — the AAD is verified by GCM at Open time but
	// not stored in the ciphertext. The domain separator lives in
	// the helper, not the blob.
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(azureSPAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, azureSPEnvelopeV1)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealAzureClientSecret reverses SealAzureClientSecret. v0.89.51
// (#674 slice 1 chunk 1).
//
// Returns an error wrapping ErrAzureSPMalformed when the blob shape
// doesn't match the envelope (truncated, unknown version byte) —
// these are typically schema-mismatch signals (operator downgraded,
// restored from backup, mis-pasted a value), not security incidents.
//
// Returns an error containing "decrypt" when the cipher rejects the
// blob — wrong SQUADRON_SECRETS_KEY (rotated), tampered ciphertext,
// or — critically — a sealed-PAT, sealed-webhook-secret, or
// sealed-GCP-SA blob attempted to be unsealed here. The domain
// separator AAD makes every cross-shape attempt fail at the auth-tag
// check, indistinguishable from any other tamper event.
//
// On success returns the plaintext SP client_secret bytes. Callers
// MUST NOT log these bytes, include them in error messages, embed
// them in audit payloads, or return them in any HTTP response. The
// only sanctioned consumer is the scanner (chunk 2) instantiating an
// azidentity.ClientSecretCredential.
func UnsealAzureClientSecret(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealAzureClientSecret: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrAzureSPMalformed)
	}
	version := blob[0]
	if version != azureSPEnvelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrAzureSPMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(azureSPAAD))
	if err != nil {
		// Match the Key.Open, UnsealWebhookSecret, and
		// UnsealGCPServiceAccount phrasing ("decrypt failed") so
		// callers that already pattern-match on that signal for PATs /
		// webhook secrets / GCP SAs see the same signal here. The
		// underlying err NEVER carries plaintext (GCM tag mismatch is
		// the only failure mode).
		return nil, fmt.Errorf("credstore: Azure SP client_secret decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
