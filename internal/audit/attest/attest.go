// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package attest provides Ed25519 signing and zero-secret verification of a
// Squadron audit-chain head (ADR 0051, the enterprise wedge of ADR 0044).
//
// The keyed HMAC chain (ADR 0044) makes the audit log tamper-EVIDENT: whoever
// holds SQUADRON_SECRETS_KEY (or the audit HMAC key) can verify it — but that
// same key-holder could also re-forge it, and the existing AES-GCM "sealed
// attestation" (ADR 0027 slice 3) is symmetric, so it proves only that the key
// holder vouches. Asymmetric Ed25519 signing closes that: the deployment signs
// the chain head with a PRIVATE key it never shares, and any auditor verifies
// with the PUBLIC key alone — zero secrets, no ability to forge.
//
// This package is pure crypto over the canonical head tip
// {tenant, head_seq, head_row_hash}. The canonical bytes are byte-identical to
// the AES seal payload (auditverify.SealPayload / the offline verifier's
// sealPayload), so the Ed25519 signature covers the exact same tip the seal
// does. The enterprise wire (ADR 0051 slice 2) calls Sign when it writes a
// checkpoint attestation; the OSS offline verifier (cmd/squadron-audit-verify
// --pubkey) and any third party call Verify.
package attest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// head is the canonical signed tip. Field order + json tags MUST match the AES
// seal payload (tenant, head_seq, head_row_hash) so the signature and the seal
// cover byte-identical bytes.
type head struct {
	Tenant      string `json:"tenant"`
	HeadSeq     int64  `json:"head_seq"`
	HeadRowHash string `json:"head_row_hash"`
}

// Canonical returns the deterministic bytes signed/verified for an audit head.
// It is byte-identical to the AES seal payload's canonical form.
func Canonical(tenant string, headSeq int64, headRowHash string) ([]byte, error) {
	return json.Marshal(head{Tenant: tenant, HeadSeq: headSeq, HeadRowHash: headRowHash})
}

// GenerateKey creates a fresh Ed25519 audit-signing keypair, returning the
// PUBLIC and PRIVATE keys as standard-base64 strings. The private key stays on
// the deployment (never shared); the public key is safe to publish to auditors.
func GenerateKey() (pubB64, privB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub),
		base64.StdEncoding.EncodeToString(priv), nil
}

// DecodePrivateKey parses a standard-base64 Ed25519 private key.
func DecodePrivateKey(privB64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// DecodePublicKey parses a standard-base64 Ed25519 public key.
func DecodePublicKey(pubB64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// Sign signs the canonical head tip with the base64 Ed25519 private key,
// returning the signature as standard-base64.
func Sign(privB64, tenant string, headSeq int64, headRowHash string) (sigB64 string, err error) {
	priv, err := DecodePrivateKey(privB64)
	if err != nil {
		return "", err
	}
	msg, err := Canonical(tenant, headSeq, headRowHash)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg)), nil
}

// ErrSignatureInvalid is returned by Verify when the signature does not match
// the head under the given public key.
var ErrSignatureInvalid = errors.New("audit head signature is invalid for this public key")

// Verify checks that sigB64 is a valid Ed25519 signature over the canonical head
// {tenant, head_seq, head_row_hash} under pubB64. Returns nil on success,
// ErrSignatureInvalid on a cryptographic mismatch, or a decode error on bad
// inputs. Zero secrets: only the PUBLIC key is needed.
func Verify(pubB64, sigB64, tenant string, headSeq int64, headRowHash string) error {
	pub, err := DecodePublicKey(pubB64)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	msg, err := Canonical(tenant, headSeq, headRowHash)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return ErrSignatureInvalid
	}
	return nil
}
