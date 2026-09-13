// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/audit/attest"
)

func signedAtt(t *testing.T) (att Attestation, pub string) {
	t.Helper()
	pub, priv, err := attest.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	att = Attestation{Tenant: "default", HeadSeq: 7, HeadRowHash: "deadbeef"}
	sig, err := attest.Sign(priv, att.Tenant, att.HeadSeq, att.HeadRowHash)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig
	att.SigKeyID = "k1"
	return att, pub
}

func TestRunSignatureCheck(t *testing.T) {
	att, pub := signedAtt(t)

	// Valid signature + correct pubkey => not failed.
	if runSignatureCheck(att, pub) {
		t.Error("valid signature with correct pubkey should not fail")
	}

	// No pubkey supplied => informational, never fails (even with a signature).
	if runSignatureCheck(att, "") {
		t.Error("no --pubkey should not fail the run")
	}

	// pubkey supplied but attestation has no signature => hard fail.
	noSig := att
	noSig.Signature = ""
	if !runSignatureCheck(noSig, pub) {
		t.Error("--pubkey with no signature in attestation should fail")
	}

	// Wrong pubkey => hard fail.
	otherPub, _, _ := attest.GenerateKey()
	if !runSignatureCheck(att, otherPub) {
		t.Error("wrong pubkey should fail")
	}

	// Tampered tip (seq changed after signing) => hard fail.
	tampered := att
	tampered.HeadSeq = 8
	if !runSignatureCheck(tampered, pub) {
		t.Error("tampered head seq should fail signature verification")
	}
}

func TestLoadPubkey_Literal(t *testing.T) {
	got, err := loadPubkey("  abc123==  ")
	if err != nil {
		t.Fatalf("loadPubkey literal: %v", err)
	}
	if got != "abc123==" {
		t.Errorf("literal pubkey should be trimmed, got %q", got)
	}
}
