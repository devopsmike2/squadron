// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// ociSigningKeyAAD is the additional-authenticated-data tag the
// SealOCIPrivateKey / UnsealOCIPrivateKey helpers bind into the
// AES-GCM authentication tag. The string is a domain separator: a
// blob sealed under this AAD will fail to open under any other AAD
// (including the nil AAD used by Key.Seal / Key.Open for PATs, the
// webhookSecretAAD used for per-connection webhook secrets, the
// gcpSAAAD used for GCP Service Account JSON, and the azureSPAAD
// used for Azure SP client_secret). v0.89.56 (#681 slice 1 chunk 1).
//
// An attacker who somehow gains read access to the oci_connections
// table cannot swap a sealed PAT, webhook secret, GCP SA JSON, or
// Azure SP client_secret into the sealed_private_key column and
// replay it as an OCI API Signing Key private key: the GCM tag was
// computed against a different AAD, so Open returns an auth-tag
// mismatch. The brief names this the critical domain-separation
// security boundary (parallel to v0.89.31's webhook-secret posture,
// v0.89.46's GCP SA posture, and v0.89.51's Azure SP posture); the
// AAD is the mechanism. This is the FOURTH sealed credential type
// — defense in depth applies across PAT, webhook secret, GCP SA,
// Azure SP client_secret, AND OCI API Signing Key private key.
//
// Private key bytes are the strongest credential type Squadron
// handles (full asymmetric authentication material). Same credstore
// sealing posture as the other three sealed credential types; same
// never-log, never-embed-in-audit, never-echo invariants.
//
// NEVER change this string after launch — sealed blobs in operator
// databases were computed against the on-launch string. A rotation
// would require a versioned envelope (which ociSigningKeyVersion
// supports), not an in-place AAD edit.
const ociSigningKeyAAD = "squadron.oci_signing_key.v1"

// ociSigningKeyVersion prefixes every sealed OCI API Signing Key
// private key blob. One byte today; the value names the envelope
// version so a slice-N AAD or cipher rotation can land without
// ambiguously reading pre-rotation blobs. v0.89.56 (#681 slice 1
// chunk 1).
const ociSigningKeyVersion byte = 0x01

// ErrOCISigningKeyMalformed is returned by UnsealOCIPrivateKey when
// the blob fails sanity checks BEFORE the cipher is asked to open
// it — wrong envelope version, too short to hold a nonce, etc.
// These signal "the blob shape isn't a v0.89.56 OCI signing key"
// rather than "the cipher rejected the tag", so callers (and
// operators) can distinguish a schema-mismatch from a tamper /
// wrong-key event.
var ErrOCISigningKeyMalformed = errors.New("credstore: OCI signing key blob is malformed")

// SealOCIPrivateKey encrypts an OCI API Signing Key private key
// (PEM-encoded RSA) for at-rest storage in
// oci_connections.sealed_private_key. v0.89.56 (#681 slice 1 chunk
// 1).
//
// Uses the same AES-256-GCM substrate the PAT, webhook-secret, GCP
// SA, and Azure SP paths use — same SQUADRON_SECRETS_KEY, same nonce
// strategy, same authenticated encryption guarantees — but pins a
// distinct AAD (ociSigningKeyAAD) so a sealed PAT, webhook secret,
// GCP SA JSON, or Azure SP client_secret can NEVER be mis-unsealed
// as an OCI private key and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// The version byte is read by UnsealOCIPrivateKey so a future
// envelope rotation does not collide with pre-rotation blobs in
// operator databases.
//
// Returns an error if key is nil, plaintext is empty (caller bug —
// the create handler validates empty-key separately), or the cipher
// fails. Errors NEVER carry the plaintext bytes; the substrate's
// no-credential-in-errors invariant extends to the OCI signing key
// path. The plaintext private key is NEVER logged, NEVER embedded
// in audit payloads, and NEVER returned in HTTP responses — the
// seal / unseal pair is the only sanctioned access path. Private
// key bytes are the strongest credential type Squadron handles
// (full asymmetric authentication material); these invariants are
// non-negotiable.
func SealOCIPrivateKey(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealOCIPrivateKey: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealOCIPrivateKey: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: OCI signing key nonce generation failed: %w", err)
	}
	// AAD-bound Seal — the AAD is verified by GCM at Open time but
	// not stored in the ciphertext. The domain separator lives in
	// the helper, not the blob.
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(ociSigningKeyAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, ociSigningKeyVersion)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealOCIPrivateKey reverses SealOCIPrivateKey. v0.89.56 (#681
// slice 1 chunk 1).
//
// Returns an error wrapping ErrOCISigningKeyMalformed when the blob
// shape doesn't match the envelope (truncated, unknown version byte)
// — these are typically schema-mismatch signals (operator
// downgraded, restored from backup, mis-pasted a value), not
// security incidents.
//
// Returns an error containing "decrypt" when the cipher rejects the
// blob — wrong SQUADRON_SECRETS_KEY (rotated), tampered ciphertext,
// or — critically — a sealed-PAT, sealed-webhook-secret, sealed-GCP-
// SA, or sealed-Azure-SP blob attempted to be unsealed here. The
// domain separator AAD makes every cross-shape attempt fail at the
// auth-tag check, indistinguishable from any other tamper event.
//
// On success returns the plaintext OCI API Signing Key private key
// (PEM-encoded RSA) bytes. Callers MUST NOT log these bytes,
// include them in error messages, embed them in audit payloads, or
// return them in any HTTP response. The only sanctioned consumer is
// the scanner (chunk 2) constructing an OCI ConfigurationProvider /
// signing each request. Private key bytes are the strongest
// credential type Squadron handles.
func UnsealOCIPrivateKey(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealOCIPrivateKey: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrOCISigningKeyMalformed)
	}
	version := blob[0]
	if version != ociSigningKeyVersion {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrOCISigningKeyMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(ociSigningKeyAAD))
	if err != nil {
		// Match the Key.Open, UnsealWebhookSecret,
		// UnsealGCPServiceAccount, and UnsealAzureClientSecret
		// phrasing ("decrypt failed") so callers that already
		// pattern-match on that signal for PATs / webhook secrets /
		// GCP SAs / Azure SPs see the same signal here. The
		// underlying err NEVER carries plaintext (GCM tag mismatch
		// is the only failure mode).
		return nil, fmt.Errorf("credstore: OCI signing key decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
