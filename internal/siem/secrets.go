// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package siem ships audit events out to external SIEM systems
// (Splunk HEC, generic signed webhooks). Added in v0.50 because
// utilities and other regulated environments need centralized 3-7
// year audit retention — Squadron's local SQLite is fine for
// operational visibility but doesn't meet compliance retention
// requirements.
//
// Architecture:
//   - AuditService writes to local SQLite synchronously (unchanged)
//   - Each write also enqueues the event on a per-destination
//     bounded channel
//   - Worker goroutines per destination drain the channel and call
//     the configured Exporter; transient failures retry with
//     exponential backoff
//   - On full queue: drop the event and bump a metric. We never
//     block audit writes — losing a few SIEM events is recoverable
//     (the local DB has the source of truth); blocking the audit
//     hot path would freeze rollouts and approvals
//
// Security:
//   - Splunk HEC tokens and webhook HMAC secrets are encrypted at
//     rest with NaCl secretbox keyed on SQUADRON_SIEM_KEY (separate
//     from SQUADRON_DEPLOY_KEY so the two surfaces can be rotated
//     independently)
//   - Plaintext never appears in API responses; the UI sees
//     has_secret: true/false
//   - Webhook bodies are HMAC-SHA256 signed and posted with an
//     X-Squadron-Signature header so receivers can verify provenance
package siem

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/nacl/secretbox"
)

// KeyEnvVar holds the base64-encoded 32-byte secretbox key. Operators
// generate one with `head -c 32 /dev/urandom | base64` and set it
// once per install. Rotating requires re-entering every destination's
// secret (the ciphertext can't be re-keyed without the old plaintext,
// which Squadron has thrown away).
const KeyEnvVar = "SQUADRON_SIEM_KEY"

const (
	keyLen   = 32
	nonceLen = 24
)

// ErrKeyMissing means the env var is unset or malformed. Callers
// should treat this as "SIEM feature disabled" and continue running —
// the local audit log still works.
var ErrKeyMissing = fmt.Errorf("SQUADRON_SIEM_KEY env var is unset or malformed; SIEM export disabled")

// Crypter wraps secretbox encrypt/decrypt so the service layer
// doesn't import nacl directly.
type Crypter struct {
	key [keyLen]byte
}

// NewCrypterFromEnv loads the key from SQUADRON_SIEM_KEY. Returns
// ErrKeyMissing when the env var is missing or wrong length.
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

// Encrypt seals plaintext with a fresh random nonce.
// Output format: nonce(24) || ciphertext.
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

// Decrypt opens a sealed blob.
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
