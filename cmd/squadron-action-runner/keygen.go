// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/actions"
)

// newKeygenCmd bootstraps the trust material needed to enroll a runner. It
// generates a fresh Ed25519 signing seed for the Squadron control plane and a
// separate keypair for the runner, then prints the three values an operator
// needs, each in the exact format its consumer expects:
//
//   - SQUADRON_ACTION_SIGNING_KEY: base64 seed set on the Squadron deployment.
//     Squadron derives its signing key from it; a persistent value means runner
//     enrollments survive Squadron restarts (an unset key is regenerated every
//     boot, which invalidates every runner).
//   - squadron_public_key_pem: the control plane's public key, pinned in the
//     runner config so a swapped Squadron can't take the runner over.
//   - private_key_pem: the runner's own key.
//
// The seed and the runner private key are secrets — capture them straight into
// a secret store, don't paste them into shared logs.
func newKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen",
		Short: "Generate the signing seed + runner key material to enroll a runner.",
		Long: "Generates a fresh Ed25519 signing seed for Squadron and a runner " +
			"keypair, and prints the three values needed to wire them together. " +
			"Run once per trust domain and store the secrets appropriately.",
		RunE: runKeygen,
	}
}

func runKeygen(_ *cobra.Command, _ []string) error {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("generate seed: %w", err)
	}
	signer, err := actions.NewSigner(seed)
	if err != nil {
		return fmt.Errorf("derive signer from seed: %w", err)
	}
	runnerPrivPEM, _, err := generatePrivateKey()
	if err != nil {
		return fmt.Errorf("generate runner key: %w", err)
	}

	fmt.Print("# ===== Squadron control plane (SECRET) =====\n")
	fmt.Print("# Set as SQUADRON_ACTION_SIGNING_KEY on the Squadron deployment.\n")
	fmt.Printf("SQUADRON_ACTION_SIGNING_KEY=%s\n\n", base64.StdEncoding.EncodeToString(seed))

	fmt.Print("# ===== Runner config (config.yaml) =====\n")
	fmt.Print("# squadron_public_key_pem pins the control plane (safe to share).\n")
	fmt.Print("# private_key_pem is this runner's own key (SECRET).\n")
	fmt.Printf("squadron_public_key_pem: |\n%s", indentYAMLBlock(signer.PublicKeyPEM()))
	fmt.Printf("private_key_pem: |\n%s", indentYAMLBlock(runnerPrivPEM))
	return nil
}

// indentYAMLBlock indents each line of a PEM string by two spaces so it drops
// cleanly under a YAML block scalar (`key: |`).
func indentYAMLBlock(pem string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(pem, "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
