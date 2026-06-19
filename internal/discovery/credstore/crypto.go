// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

// EnvVarSecretsKey is the environment variable Squadron reads the
// substrate's data-encryption key from. The value is base64-encoded
// raw bytes; the decoded length must be exactly 32 (AES-256).
const EnvVarSecretsKey = "SQUADRON_SECRETS_KEY"

// keyByteLen is the required decoded length of SQUADRON_SECRETS_KEY.
// AES-256-GCM uses a 256-bit key, so the decoded value must be exactly
// 32 bytes. Anything shorter or longer is rejected — no key stretching,
// no zero-padding, no fallback.
const keyByteLen = 32

// nonceByteLen is the nonce length used with AES-GCM. The standard 12
// is what crypto/cipher.NewGCM returns from NonceSize() and is the
// only size we ever generate or accept.
const nonceByteLen = 12

// ErrSecretsKeyMissing is returned when SQUADRON_SECRETS_KEY is unset
// or empty. The substrate refuses to construct in this state so an
// operator who forgot to set the key sees a hard failure at startup
// rather than discovering it later when the on-disk rows are
// effectively plaintext.
var ErrSecretsKeyMissing = errors.New(
	"credstore: " + EnvVarSecretsKey + " is not set; the credential substrate refuses to start without a key",
)

// ErrSecretsKeyMalformed is returned when SQUADRON_SECRETS_KEY is set
// but does not base64-decode to exactly 32 bytes. Tolerating a wrong
// length would either weaken the cipher (truncate) or accept silent
// corruption (pad with zeros); we do neither.
var ErrSecretsKeyMalformed = errors.New(
	"credstore: " + EnvVarSecretsKey + " must be base64-encoded 32 bytes (AES-256)",
)

// loadKeyFromEnv reads SQUADRON_SECRETS_KEY, base64-decodes it, and
// returns the raw 32-byte key. Any deviation from the contract — env
// missing, not base64, wrong length — is a hard error.
func loadKeyFromEnv() ([]byte, error) {
	raw := os.Getenv(EnvVarSecretsKey)
	if raw == "" {
		return nil, ErrSecretsKeyMissing
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Wrap with the sentinel so callers can errors.Is against it
		// without leaking the raw env-var contents into the error.
		return nil, fmt.Errorf("%w: %v", ErrSecretsKeyMalformed, err)
	}
	if len(key) != keyByteLen {
		return nil, fmt.Errorf("%w: got %d bytes, need %d",
			ErrSecretsKeyMalformed, len(key), keyByteLen)
	}
	return key, nil
}

// cryptor wraps a configured cipher.AEAD and exposes seal/open against
// freshly-generated nonces. One per Store; safe for concurrent use
// because AEAD operations don't share mutable state.
type cryptor struct {
	aead cipher.AEAD
}

// newCryptor builds a cryptor from a 32-byte raw key. Returns an error
// only if the key length is wrong (the only other failure modes from
// aes.NewCipher and cipher.NewGCM are length-related and crypto/cipher
// internal invariants).
func newCryptor(key []byte) (*cryptor, error) {
	if len(key) != keyByteLen {
		return nil, fmt.Errorf("%w: cryptor needs %d bytes, got %d",
			ErrSecretsKeyMalformed, keyByteLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credstore: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credstore: cipher.NewGCM: %w", err)
	}
	if aead.NonceSize() != nonceByteLen {
		// Defensive: AES-GCM is documented to use 12-byte nonces but
		// the package surface allows custom sizes. If something
		// upstream changes, fail loud rather than silently switching.
		return nil, fmt.Errorf("credstore: unexpected AES-GCM nonce size %d, want %d",
			aead.NonceSize(), nonceByteLen)
	}
	return &cryptor{aead: aead}, nil
}

// seal encrypts plaintext under a fresh random nonce and returns
// (ciphertext, nonce). Each call uses a new nonce so reusing the same
// plaintext (e.g., a re-stored row) yields different on-disk bytes.
// The ciphertext includes the GCM authentication tag; tampering with
// either field will make open fail.
func (c *cryptor) seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("credstore: nonce generation failed: %w", err)
	}
	ciphertext = c.aead.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// open decrypts ciphertext under the stored nonce. Returns an error
// containing "decrypt" if the GCM authentication tag fails — tampering
// with the ciphertext, the nonce, or the key all produce this path.
// The substrate's tamper-detection test verifies this contract.
func (c *cryptor) open(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != nonceByteLen {
		return nil, fmt.Errorf("credstore: decrypt failed: nonce length %d, want %d",
			len(nonce), nonceByteLen)
	}
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Wrap with the literal "decrypt" so tests and operators see a
		// consistent signal regardless of the underlying cipher's
		// internal error message.
		return nil, fmt.Errorf("credstore: decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}
