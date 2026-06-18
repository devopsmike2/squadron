// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/devopsmike2/squadron/internal/actions"
)

// Config is what the operator drops in /etc/squadron/action-runner.yaml
// (or anywhere --config points to). YAML rather than JSON because
// the runner is operator-facing and YAML is more forgiving of
// comments and multi-line capability lists.
type Config struct {
	// RunnerID is the stable identifier the runner publishes at
	// registration. By convention it is the Squadron-side
	// fingerprint of the runner's own public key (ed25519:<short>),
	// but any non-empty string the operator picks works.
	RunnerID string `yaml:"runner_id"`

	// Hostname is what shows up in the UI. Defaults to os.Hostname.
	Hostname string `yaml:"hostname"`

	// PrivateKeyPEM is the runner's own Ed25519 private key in PEM
	// form. Future mutual-auth work uses this; the MVP runner only
	// uses HTTPS to authenticate to Squadron.
	PrivateKeyPEM string `yaml:"private_key_pem"`

	// SquadronPublicKeyPEM is pinned at install time. The runner
	// rejects any action request whose signature does not verify
	// against this key. A swapped Squadron instance cannot quietly
	// take over an existing runner — the operator has to
	// reinstall.
	SquadronPublicKeyPEM string `yaml:"squadron_public_key_pem"`

	// SquadronURL is the base URL of the Squadron API
	// (https://squadron.internal/api/v1). The runner appends paths
	// (/runners/register etc.) at use time.
	SquadronURL string `yaml:"squadron_url"`

	// AuthToken is a Squadron-issued bearer token carrying
	// actions:write. Issued at enrollment time in production; in
	// dev with auth disabled this can be empty.
	AuthToken string `yaml:"auth_token"`

	// Capabilities declares what the runner is allowed to do.
	// Squadron's dispatch handler refuses to send requests outside
	// this set; the runner refuses to execute them too (defense in
	// depth).
	Capabilities []actions.Capability `yaml:"capabilities"`

	// PollInterval is how often the runner asks Squadron for
	// pending work. 5s is a reasonable default for demos; production
	// can tune to 30s or use long-poll once that endpoint exists.
	PollInterval time.Duration `yaml:"poll_interval"`
}

// LoadConfig reads and parses the runner's YAML config.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.RunnerID == "" {
		return nil, fmt.Errorf("runner_id is required")
	}
	if c.SquadronURL == "" {
		return nil, fmt.Errorf("squadron_url is required")
	}
	if c.SquadronPublicKeyPEM == "" {
		return nil, fmt.Errorf("squadron_public_key_pem is required")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.Hostname == "" {
		h, _ := os.Hostname()
		c.Hostname = h
	}
	return &c, nil
}

// privateKeyFromPEM parses an Ed25519 private key from the PEM
// block format. The runner uses this to derive its own public key
// at registration time (so Squadron can later validate result
// posts in a future mutual-auth flow).
func privateKeyFromPEM(pemStr string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block in private_key_pem")
	}
	if len(block.Bytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key wrong size: %d", len(block.Bytes))
	}
	return ed25519.PrivateKey(block.Bytes), nil
}

// generatePrivateKey is a small helper for the init flow (future
// SQ-2.4 polish): produces a fresh PEM-encoded private key so the
// operator can paste it into config.
func generatePrivateKey() (string, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	block := &pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: priv}
	return string(pem.EncodeToMemory(block)), pub, nil
}

// encodePublicKeyPEM returns the public half of a private key in
// the same PEM block shape Squadron's signer uses. Returns an empty
// string if the supplied private key has no public key (cannot
// happen for Ed25519 but keeps the helper tolerant).
func encodePublicKeyPEM(priv ed25519.PrivateKey) string {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || len(pub) == 0 {
		return ""
	}
	block := &pem.Block{Type: "ED25519 PUBLIC KEY", Bytes: pub}
	return string(pem.EncodeToMemory(block))
}
