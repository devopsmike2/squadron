// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package attest

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sig, err := Sign(priv, "default", 42, "abc123")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(pub, sig, "default", 42, "abc123"); err != nil {
		t.Fatalf("Verify round-trip should pass: %v", err)
	}
}

func TestVerify_TamperedTip(t *testing.T) {
	pub, priv, _ := GenerateKey()
	sig, _ := Sign(priv, "default", 42, "abc123")

	cases := []struct {
		name   string
		tenant string
		seq    int64
		hash   string
	}{
		{"seq changed", "default", 43, "abc123"},
		{"hash changed", "default", 42, "abc124"},
		{"tenant changed", "other", 42, "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(pub, sig, tc.tenant, tc.seq, tc.hash)
			if !errors.Is(err, ErrSignatureInvalid) {
				t.Errorf("tampered %s: want ErrSignatureInvalid, got %v", tc.name, err)
			}
		})
	}
}

func TestVerify_WrongKey(t *testing.T) {
	_, priv, _ := GenerateKey()
	otherPub, _, _ := GenerateKey()
	sig, _ := Sign(priv, "default", 1, "h")
	if err := Verify(otherPub, sig, "default", 1, "h"); !errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("wrong key: want ErrSignatureInvalid, got %v", err)
	}
}

func TestVerify_BadInputs(t *testing.T) {
	pub, priv, _ := GenerateKey()
	sig, _ := Sign(priv, "default", 1, "h")

	if err := Verify("not-base64!!", sig, "default", 1, "h"); err == nil {
		t.Error("bad pubkey base64 should error")
	}
	if err := Verify(pub, "not-base64!!", "default", 1, "h"); err == nil {
		t.Error("bad signature base64 should error")
	}
	if err := Verify("YWJj", sig, "default", 1, "h"); err == nil {
		t.Error("wrong-length pubkey should error")
	}
}

// TestCanonical_MatchesSealPayload guards the byte-for-byte compatibility with
// the AES seal payload: the Ed25519 signature MUST cover the same canonical tip
// {tenant, head_seq, head_row_hash} that the seal does, or the two attestations
// would disagree.
func TestCanonical_MatchesSealPayload(t *testing.T) {
	got, err := Canonical("default", 42, "abc123")
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	// Mirror of cmd/squadron-audit-verify sealPayload{Tenant,HeadSeq,HeadRowHash}.
	want, _ := json.Marshal(struct {
		Tenant      string `json:"tenant"`
		HeadSeq     int64  `json:"head_seq"`
		HeadRowHash string `json:"head_row_hash"`
	}{"default", 42, "abc123"})
	if string(got) != string(want) {
		t.Errorf("canonical mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestDecodeKey_LengthGuards(t *testing.T) {
	if _, err := DecodePrivateKey("YWJj"); err == nil {
		t.Error("short private key should error")
	}
	if _, err := DecodePublicKey("YWJj"); err == nil {
		t.Error("short public key should error")
	}
}
