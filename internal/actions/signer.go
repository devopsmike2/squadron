// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// Signer wraps an Ed25519 private key. Squadron's control plane
// holds one Signer; every action Request flows through Sign before
// going out. Runners hold the corresponding Verifier (just the
// public key plus the algorithm).
//
// Key rotation is in scope but not in this MVP. v1 ships with a
// single key per Squadron instance; v2 will add rotating key sets
// and a JWKS endpoint runners can poll. Until then, key rotation
// means installing the new public key on every runner before
// rotating Squadron's private key.
type Signer struct {
	priv ed25519.PrivateKey
}

// NewSigner constructs a Signer from a 32-byte Ed25519 seed. The
// caller is responsible for loading the seed from a secure source
// (an env var, a Vault read, an HSM proxy). Returning an error for
// the wrong seed length lets the caller surface a clean config
// problem rather than a runtime panic.
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ed25519 seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv}, nil
}

// GenerateSigner produces a Signer with a fresh random key. Useful
// for tests and the bootstrap flow when a fresh Squadron install
// has no prior signing key configured.
func GenerateSigner() (*Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signer: %w", err)
	}
	return &Signer{priv: priv}, nil
}

// PublicKey returns the Ed25519 public key bytes. Runners pin this
// at install time and use it to verify every incoming Request.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// PublicKeyPEM returns the public key serialized as PEM. This is
// the form Squadron embeds in its enrollment payload and the form
// runner configs store on disk.
func (s *Signer) PublicKeyPEM() string {
	block := &pem.Block{
		Type:  "ED25519 PUBLIC KEY",
		Bytes: s.PublicKey(),
	}
	return string(pem.EncodeToMemory(block))
}

// Fingerprint returns a short, human-readable identifier of the
// public key. Used in audit logs and the UI so operators can
// confirm "yes, this is the Squadron we trust" without staring at
// 32 bytes of base64.
func (s *Signer) Fingerprint() string {
	sum := sha256.Sum256(s.PublicKey())
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(sum[:8])
}

// Sign produces a base64-encoded signature for the supplied
// request. The request's Signature field is set in place and also
// returned, so callers can use whichever pattern reads cleanest at
// their call site.
//
// Sign assumes RequestID, ProposalID, RunnerID, Action.Type,
// Action.Parameters, IssuedAt, ExpiresAt, and Phase are populated.
// If IssuedAt is zero it is set to time.Now().UTC() and ExpiresAt
// defaults to IssuedAt + 5 minutes if also zero, matching the
// design doc's replay window.
func (s *Signer) Sign(r *Request) (string, error) {
	if r == nil {
		return "", errors.New("nil request")
	}
	if r.IssuedAt.IsZero() {
		r.IssuedAt = time.Now().UTC()
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = r.IssuedAt.Add(5 * time.Minute)
	}
	bytes, err := r.signingBytes()
	if err != nil {
		return "", fmt.Errorf("serialize: %w", err)
	}
	sig := ed25519.Sign(s.priv, bytes)
	r.Signature = base64.RawURLEncoding.EncodeToString(sig)
	return r.Signature, nil
}

// Verifier is what a runner holds. It wraps the issuer's public
// key and performs Verify on every incoming Request. Construct
// from a PEM block (the operator pasted Squadron's public key into
// the runner config at install time) or directly from a raw
// ed25519.PublicKey when wiring in process for tests.
type Verifier struct {
	pub ed25519.PublicKey
}

// NewVerifier constructs a Verifier from a raw public key.
func NewVerifier(pub ed25519.PublicKey) (*Verifier, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key must be %d bytes, got %d", ed25519.PublicKeySize, len(pub))
	}
	return &Verifier{pub: pub}, nil
}

// NewVerifierFromPEM parses a PEM-encoded Ed25519 public key and
// returns a Verifier. Matches the format Signer.PublicKeyPEM
// produces.
func NewVerifierFromPEM(pemBytes []byte) (*Verifier, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("verifier: no PEM block found")
	}
	if block.Type != "ED25519 PUBLIC KEY" {
		return nil, fmt.Errorf("verifier: unexpected PEM block type %q", block.Type)
	}
	return NewVerifier(ed25519.PublicKey(block.Bytes))
}

// Verify checks the request's signature against the verifier's
// public key and confirms the request hasn't expired. Returns nil
// on success; a non-nil error describes the rejection reason.
//
// Replay protection: an expired ExpiresAt rejects the request
// outright, even if the signature is valid. The 5-minute default
// in Signer.Sign means a captured request becomes useless quickly.
func (v *Verifier) Verify(r *Request, now time.Time) error {
	if r == nil {
		return errors.New("nil request")
	}
	if r.Signature == "" {
		return errors.New("missing signature")
	}
	if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
		return fmt.Errorf("request expired at %s (now %s)", r.ExpiresAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	bytes, err := r.signingBytes()
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}
	if !ed25519.Verify(v.pub, bytes, sig) {
		return errors.New("signature verification failed")
	}
	return nil
}
