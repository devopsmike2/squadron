// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// oidcLoginCookieAAD is the additional-authenticated-data tag the
// SealOIDCLoginCookie / UnsealOIDCLoginCookie helpers bind into the
// AES-GCM authentication tag. The string is a domain separator: the
// sealed login-cookie payload (state, nonce, connID, returnTo) fails
// to open under any other AAD (PAT nil AAD, webhook secret AAD, OIDC
// client secret AAD). ADR 0014 D7 — one cookie sealing the OIDC
// login handshake state, providing confidentiality + integrity in
// one primitive with no hand-rolled HMAC.
//
// NEVER change this string after launch — a rotation would require a
// versioned envelope (which oidcLoginCookieEnvelopeV1 supports), not
// an in-place AAD edit. (In practice login cookies are short-lived —
// 600s — so a rotation only breaks in-flight logins, but the versioned
// envelope is kept for consistency with the sibling seal helpers.)
const oidcLoginCookieAAD = "squadron.oidc_login_cookie.v1"

// oidcLoginCookieEnvelopeV1 prefixes every sealed OIDC-login-cookie
// blob. One byte today; the value names the envelope version so a
// later AAD or cipher rotation can land without ambiguously reading
// pre-rotation blobs. ADR 0014 D7.
const oidcLoginCookieEnvelopeV1 byte = 0x01

// ErrOIDCLoginCookieMalformed is returned by UnsealOIDCLoginCookie
// when the blob fails sanity checks BEFORE the cipher is asked to
// open it — wrong envelope version, too short to hold a nonce, etc.
// Signals a schema-mismatch (an old/foreign cookie) rather than a
// tamper / wrong-key event.
var ErrOIDCLoginCookieMalformed = errors.New("credstore: oidc login cookie blob is malformed")

// SealOIDCLoginCookie encrypts the OIDC login-handshake payload
// (marshaled {state, nonce, connID, returnTo}) for storage in the
// short-lived squadron_oidc_login cookie. ADR 0014 D7.
//
// Uses the same AES-256-GCM substrate the PAT/webhook/client-secret
// paths use — same SQUADRON_SECRETS_KEY, same nonce strategy — but
// pins a distinct AAD (oidcLoginCookieAAD) so a cookie blob can
// NEVER be mis-unsealed as another credential shape and vice versa.
//
// The output envelope is:
//
//	[1 byte envelope version][12-byte nonce][ciphertext+tag]
//
// Returns an error if key is nil, plaintext is empty, or the cipher
// fails. Errors NEVER carry the plaintext bytes.
func SealOIDCLoginCookie(key *Key, plaintext []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: SealOIDCLoginCookie: key is required")
	}
	if len(plaintext) == 0 {
		return nil, errors.New("credstore: SealOIDCLoginCookie: plaintext is required")
	}
	nonce := make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credstore: oidc login cookie nonce generation failed: %w", err)
	}
	ciphertext := key.aead.Seal(nil, nonce, plaintext, []byte(oidcLoginCookieAAD))
	blob := make([]byte, 0, 1+nonceByteLen+len(ciphertext))
	blob = append(blob, oidcLoginCookieEnvelopeV1)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnsealOIDCLoginCookie reverses SealOIDCLoginCookie. ADR 0014 D7.
//
// Returns an error wrapping ErrOIDCLoginCookieMalformed when the blob
// shape doesn't match the envelope (truncated, unknown version byte).
//
// Returns an error containing "decrypt" when the cipher rejects the
// blob — tampered cookie, wrong SQUADRON_SECRETS_KEY, or a cross-AAD
// blob. This is the CSRF/tamper defense: a forged or edited cookie
// fails the GCM tag check.
//
// On success returns the plaintext bytes (the marshaled payload).
func UnsealOIDCLoginCookie(key *Key, blob []byte) ([]byte, error) {
	if key == nil {
		return nil, errors.New("credstore: UnsealOIDCLoginCookie: key is required")
	}
	if len(blob) < 1+nonceByteLen {
		return nil, fmt.Errorf("%w: blob shorter than envelope header", ErrOIDCLoginCookieMalformed)
	}
	version := blob[0]
	if version != oidcLoginCookieEnvelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version 0x%02x", ErrOIDCLoginCookieMalformed, version)
	}
	nonce := blob[1 : 1+nonceByteLen]
	ciphertext := blob[1+nonceByteLen:]
	plaintext, err := key.aead.Open(nil, nonce, ciphertext, []byte(oidcLoginCookieAAD))
	if err != nil {
		return nil, fmt.Errorf("credstore: oidc login cookie decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
