// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/actions"
)

// TestKeygenMaterial_RoundTrips proves keygen emits a matched pair: the seed it
// base64-encodes as SQUADRON_ACTION_SIGNING_KEY and the squadron_public_key_pem
// it prints are two halves of one Ed25519 key. A signer built from the seed
// must produce signatures a verifier built from the emitted PEM accepts —
// otherwise a runner enrolled with the PEM would reject every real dispatch.
func TestKeygenMaterial_RoundTrips(t *testing.T) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	signer, err := actions.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer from seed: %v", err)
	}

	verifier, err := actions.NewVerifierFromPEM([]byte(signer.PublicKeyPEM()))
	if err != nil {
		t.Fatalf("verifier from emitted PEM: %v", err)
	}

	req := &actions.Request{
		RequestID: "req-1",
		RunnerID:  "runner-1",
		Action: actions.ActionPayload{
			Type:       actions.RestartK8sWorkloadType,
			Parameters: []byte(`{"namespace":"ns","name":"x"}`),
		},
		IssuedAt:  time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Minute).UTC(),
		Phase:     actions.PhaseExecute,
	}
	sig, err := signer.Sign(req)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req.Signature = sig
	if err := verifier.Verify(req, time.Now().UTC()); err != nil {
		t.Fatalf("verifier must accept the signer's output: %v", err)
	}
}

func TestIndentYAMLBlock(t *testing.T) {
	got := indentYAMLBlock("line1\nline2\n")
	want := "  line1\n  line2\n"
	if got != want {
		t.Fatalf("indent = %q, want %q", got, want)
	}
}
