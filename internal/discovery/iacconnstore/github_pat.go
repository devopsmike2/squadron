// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// GitHubPATCredentials is the slice-1 plaintext shape for a GitHub
// PAT-authenticated IaCConnection. The token is a classic PAT scoped
// `repo`. Lives inside the encrypted blob exclusively — every other
// field of the IaCConnection is plaintext on disk, but the token is
// never persisted, logged, or emitted in an audit payload in
// plaintext form.
//
// The shape parallels credstore.AWSCredentials: a small struct with
// the secret field, marshaled to JSON and sealed by the credstore
// Key. The Marshal/Unmarshal helpers below are deliberately the
// only code path that ever holds the plaintext token.
type GitHubPATCredentials struct {
	// Token is the classic GitHub PAT, scope `repo`. Never log this
	// field. Never include it in an error message. Never include it
	// in an audit payload.
	Token string `json:"token"`
}

// nonceLen is the AES-GCM nonce length the credstore Key uses (12
// bytes). Duplicated as a local constant rather than imported so a
// future change to credstore's internal nonce size is caught at
// compile time by the marshal/unmarshal round-trip tests rather than
// silently propagating.
const nonceLen = 12

// errCredMarshalFailed is the opaque error returned when marshaling
// or sealing fails. The error string deliberately does NOT carry the
// underlying cause (which could be a JSON-encoding error with a
// stringified token in some pathological future) — the substrate's
// no-token-in-errors invariant is enforced at this boundary.
var errCredMarshalFailed = errors.New("iacconnstore: cred-marshal-failed")

// MarshalGitHubPATCreds serializes creds and seals the payload with
// the supplied credstore Key. Returns a single opaque blob containing
// the AES-GCM nonce prefixed to the ciphertext: [12-byte nonce][ct].
// The blob is what callers assign to IaCConnection.CredCiphertext.
//
// The single-blob shape (vs. credstore's separate ciphertext+nonce
// columns) matches the IaCConnection.CredCiphertext spec. The split
// is the substrate's internal detail; the GitHub-client wrapper in
// phase 2 calls UnmarshalGitHubPATCreds without knowing the layout.
//
// Returns errCredMarshalFailed (errors.Is-comparable) on any failure
// path. The error never carries the token bytes, the JSON marshal
// output, or the cipher's underlying error message.
func MarshalGitHubPATCreds(creds GitHubPATCredentials, key *credstore.Key) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: key is required", errCredMarshalFailed)
	}
	if creds.Token == "" {
		// Empty token is a validation error, not a marshal error —
		// the wizard should have rejected it before reaching here.
		// Returning a typed error so callers can distinguish "the
		// operator submitted an empty field" from "the cipher
		// failed".
		return nil, fmt.Errorf("%w: Token is required", errCredMarshalFailed)
	}
	plaintext, err := json.Marshal(creds)
	if err != nil {
		// Defensive: json.Marshal of a struct with a single string
		// field cannot fail in practice, but if it ever did we
		// MUST NOT propagate err (it may contain the token bytes).
		return nil, errCredMarshalFailed
	}
	ciphertext, nonce, err := key.Seal(plaintext)
	if err != nil {
		// Same posture: do not propagate the underlying cipher
		// error. The substrate's invariant is no-token-in-errors;
		// the cipher's internal failure modes are not interesting
		// to operators.
		return nil, errCredMarshalFailed
	}
	if len(nonce) != nonceLen {
		// credstore.Key.Seal always returns a 12-byte nonce per
		// the AES-GCM contract. Belt-and-braces check so a future
		// refactor that changes nonce size is caught here rather
		// than silently mis-packing the blob.
		return nil, errCredMarshalFailed
	}
	blob := make([]byte, 0, nonceLen+len(ciphertext))
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnmarshalGitHubPATCreds unpacks blob, decrypts the ciphertext with
// the supplied Key, and JSON-decodes the result back into a
// GitHubPATCredentials struct.
//
// blob is the format MarshalGitHubPATCreds produces: a 12-byte AES-
// GCM nonce followed by the sealed ciphertext. Any decrypt failure
// (wrong key, tampered ciphertext, truncated blob) produces an error
// whose message contains "decrypt" so callers can match against a
// stable signal — and which never carries the token bytes (the
// plaintext is discarded before the error is constructed in the
// no-token-leak invariant).
func UnmarshalGitHubPATCreds(blob []byte, key *credstore.Key) (*GitHubPATCredentials, error) {
	if key == nil {
		return nil, errors.New("iacconnstore: UnmarshalGitHubPATCreds: key is required")
	}
	if len(blob) < nonceLen {
		// Too short to even hold a nonce — definitely tampered or
		// truncated. Surface as a decrypt failure so callers can
		// match the same way they do for a tag mismatch.
		return nil, errors.New("iacconnstore: decrypt failed: blob shorter than nonce length")
	}
	nonce := blob[:nonceLen]
	ciphertext := blob[nonceLen:]
	plaintext, err := key.Open(ciphertext, nonce)
	if err != nil {
		// credstore.Key.Open already wraps with "decrypt failed".
		// Re-wrap with our package prefix; the inner message
		// preserves the "decrypt" signal.
		return nil, fmt.Errorf("iacconnstore: %w", err)
	}
	var creds GitHubPATCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		// Same no-leak posture as Marshal: do not propagate err
		// from json.Unmarshal — it may quote bytes of the plaintext
		// in its error message.
		return nil, errors.New("iacconnstore: decrypt failed: plaintext is not a valid GitHubPATCredentials JSON")
	}
	return &creds, nil
}
