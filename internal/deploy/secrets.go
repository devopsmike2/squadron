// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package deploy is the v0.34 deployment-trigger surface. It lets
// Squadron fire GitHub Actions workflows (workflow_dispatch),
// track run status, and close the inventory loop by auto-registering
// expected hosts after a successful deploy.
//
// Security model:
//   - PAT tokens are encrypted at rest with NaCl secretbox keyed on
//     a SQUADRON_DEPLOY_KEY environment variable. If the env var is
//     missing the deploy package refuses to start — no plaintext fallback.
//   - Each token gets a fresh random nonce; format on disk is
//     nonce(24) || ciphertext.
//   - The cleartext token never appears in API responses; the UI
//     only sees has_credential: true/false.
//
// The provider interface keeps the door open for non-GitHub deploys
// (Jenkins, GitLab) without leaking GitHub-specific assumptions
// into the service layer.
package deploy

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/nacl/secretbox"
)

// KeyEnvVar is the env var holding the base64-encoded 32-byte
// secretbox key. Operators generate one with
//
//	head -c 32 /dev/urandom | base64
//
// and set it once per install; rotating it requires re-creating
// every deploy target (the encrypted credentials can't be re-keyed
// without the old plaintext, which Squadron has thrown away).
const KeyEnvVar = "SQUADRON_DEPLOY_KEY"

// keyLen is the secretbox key length. nonceLen is its nonce length.
const (
	keyLen   = 32
	nonceLen = 24
)

// ErrKeyMissing is returned when SQUADRON_DEPLOY_KEY is unset or
// malformed. Callers should treat this as "deploy feature disabled".
var ErrKeyMissing = fmt.Errorf("SQUADRON_DEPLOY_KEY env var is unset or malformed; deploy feature disabled")

// Crypter wraps the secretbox encrypt + decrypt operations behind
// a tiny interface so the service layer doesn't import nacl
// directly. Constructed once via NewCrypterFromEnv and shared.
type Crypter struct {
	key [keyLen]byte
}

// NewCrypterFromEnv loads the secretbox key from the
// SQUADRON_DEPLOY_KEY env var. Returns ErrKeyMissing when the key
// is missing or wrong length; callers should disable the deploy
// feature in that case rather than crash the whole process.
func NewCrypterFromEnv() (*Crypter, error) {
	raw := os.Getenv(KeyEnvVar)
	if raw == "" {
		return nil, ErrKeyMissing
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrKeyMissing
	}
	if len(decoded) != keyLen {
		return nil, ErrKeyMissing
	}
	c := &Crypter{}
	copy(c.key[:], decoded)
	return c, nil
}

// Encrypt seals a plaintext PAT (or any byte slice) with a fresh
// random nonce. Output format: nonce(24) || ciphertext. Safe to
// store as a BLOB in SQLite.
func (c *Crypter) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("crypter is nil")
	}
	var nonce [nonceLen]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	out := make([]byte, 0, nonceLen+len(plaintext)+secretbox.Overhead)
	out = append(out, nonce[:]...)
	out = secretbox.Seal(out, plaintext, &nonce, &c.key)
	return out, nil
}

// Decrypt opens a sealed blob. Returns an error on truncation or
// tampering. Callers should not log the returned plaintext.
func (c *Crypter) Decrypt(sealed []byte) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("crypter is nil")
	}
	if len(sealed) < nonceLen+secretbox.Overhead {
		return nil, fmt.Errorf("ciphertext too short")
	}
	var nonce [nonceLen]byte
	copy(nonce[:], sealed[:nonceLen])
	plaintext, ok := secretbox.Open(nil, sealed[nonceLen:], &nonce, &c.key)
	if !ok {
		return nil, fmt.Errorf("decryption failed (key changed or ciphertext tampered)")
	}
	return plaintext, nil
}
