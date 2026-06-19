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

// Key wraps the AES-256-GCM AEAD configured for the substrate. It is
// the encryption primitive shared between the SQLiteSecretsBackend
// (Store-side) and the per-provider Marshal/Unmarshal helpers
// (caller-side). The same Key instance can be used from multiple
// goroutines — crypto/cipher.AEAD operations don't share mutable state.
//
// Construct via LoadKeyFromEnv (production path) or NewKey (tests /
// callers that have already obtained the raw bytes). The substrate
// refuses to construct without a Key; there is no fallback to a default
// key, a zero key, or plaintext.
type Key struct {
	aead cipher.AEAD
}

// LoadKeyFromEnv reads SQUADRON_SECRETS_KEY, base64-decodes it, and
// returns a Key wrapping a fresh AES-256-GCM AEAD. Any deviation from
// the contract — env missing, not base64, wrong length — is a hard
// error.
//
// This is the production path the SQLiteSecretsBackend constructor
// uses. Tests construct via NewKey to skip env coupling.
func LoadKeyFromEnv() (*Key, error) {
	raw := os.Getenv(EnvVarSecretsKey)
	if raw == "" {
		return nil, ErrSecretsKeyMissing
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Wrap with the sentinel so callers can errors.Is against it
		// without leaking the raw env-var contents into the error.
		return nil, fmt.Errorf("%w: %v", ErrSecretsKeyMalformed, err)
	}
	if len(decoded) != keyByteLen {
		return nil, fmt.Errorf("%w: got %d bytes, need %d",
			ErrSecretsKeyMalformed, len(decoded), keyByteLen)
	}
	return NewKey(decoded)
}

// NewKey builds a Key from a 32-byte raw key. Returns an error only if
// the key length is wrong or the cipher package reports an internal
// invariant violation. The other failure modes from aes.NewCipher and
// cipher.NewGCM are length-related.
func NewKey(raw []byte) (*Key, error) {
	if len(raw) != keyByteLen {
		return nil, fmt.Errorf("%w: Key needs %d bytes, got %d",
			ErrSecretsKeyMalformed, keyByteLen, len(raw))
	}
	block, err := aes.NewCipher(raw)
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
	return &Key{aead: aead}, nil
}

// Seal encrypts plaintext under a fresh random nonce and returns
// (ciphertext, nonce). Each call uses a new nonce so reusing the same
// plaintext (e.g., a re-stored row) yields different on-disk bytes.
// The ciphertext includes the GCM authentication tag; tampering with
// either field will make Open fail.
func (k *Key) Seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, nonceByteLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("credstore: nonce generation failed: %w", err)
	}
	ciphertext = k.aead.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// Open decrypts ciphertext under the stored nonce. Returns an error
// containing "decrypt" if the GCM authentication tag fails — tampering
// with the ciphertext, the nonce, or the key all produce this path.
// The substrate's tamper-detection test verifies this contract.
func (k *Key) Open(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != nonceByteLen {
		return nil, fmt.Errorf("credstore: decrypt failed: nonce length %d, want %d",
			len(nonce), nonceByteLen)
	}
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Wrap with the literal "decrypt" so tests and operators see a
		// consistent signal regardless of the underlying cipher's
		// internal error message.
		return nil, fmt.Errorf("credstore: decrypt failed (auth tag mismatch): %w", err)
	}
	return plaintext, nil
}

// SecretsBackend is the substrate's pluggable encryption interface.
// The OSS edition ships with SQLiteSecretsBackend (AES-256-GCM keyed
// by SQUADRON_SECRETS_KEY). The Compliance Pack will land
// implementations backed by Vault, AWS Secrets Manager, and GCP Secret
// Manager — same interface, no schema changes needed.
//
// Implementations must be safe for concurrent use. The Store calls
// Encrypt on every write and Decrypt on every read; no internal
// caching of plaintext occurs above this layer.
type SecretsBackend interface {
	// Encrypt seals plaintext and returns (ciphertext, nonce). Both
	// outputs are stored verbatim on the substrate row. A fresh
	// nonce must be generated per call so repeated identical
	// plaintexts produce different ciphertexts.
	Encrypt(plaintext []byte) (ciphertext []byte, nonce []byte, err error)

	// Decrypt opens ciphertext using the stored nonce. Implementations
	// must surface a clear error (containing "decrypt" or "auth")
	// when the authentication tag fails so operators can distinguish
	// tamper events from other failures.
	Decrypt(ciphertext, nonce []byte) (plaintext []byte, err error)
}

// SQLiteSecretsBackend is the OSS-default SecretsBackend. It wraps a
// Key loaded from SQUADRON_SECRETS_KEY and delegates Seal / Open
// without any additional state. The Compliance Pack's Vault-backed
// equivalent will look identical from the substrate's perspective —
// only the Encrypt/Decrypt implementation differs.
type SQLiteSecretsBackend struct {
	key *Key
}

// NewSQLiteSecretsBackend constructs a SQLiteSecretsBackend with the
// supplied Key. The constructor is intentionally separate from key
// loading so tests can construct backends with deterministic keys
// without round-tripping through the env var.
func NewSQLiteSecretsBackend(key *Key) *SQLiteSecretsBackend {
	return &SQLiteSecretsBackend{key: key}
}

// Encrypt delegates to the wrapped Key. See Key.Seal.
func (s *SQLiteSecretsBackend) Encrypt(plaintext []byte) ([]byte, []byte, error) {
	return s.key.Seal(plaintext)
}

// Decrypt delegates to the wrapped Key. See Key.Open.
func (s *SQLiteSecretsBackend) Decrypt(ciphertext, nonce []byte) ([]byte, error) {
	return s.key.Open(ciphertext, nonce)
}
