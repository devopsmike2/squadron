// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrGenerateKey_GeneratesAndPersists(t *testing.T) {
	t.Setenv(EnvVarSecretsKey, "")
	dir := t.TempDir()

	k1, gen1, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !gen1 {
		t.Error("first call should report generated=true")
	}
	if k1 == nil {
		t.Fatal("nil key")
	}

	keyPath := filepath.Join(dir, SecretsKeyFileName)
	info, statErr := os.Stat(keyPath)
	if statErr != nil {
		t.Fatalf("key file not persisted: %v", statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	first, _ := os.ReadFile(keyPath)

	// Second call must LOAD the persisted key, not regenerate it.
	k2, gen2, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if gen2 {
		t.Error("second call should report generated=false (loaded persisted key)")
	}
	if k2 == nil {
		t.Fatal("nil key on reload")
	}
	second, _ := os.ReadFile(keyPath)
	if string(first) != string(second) {
		t.Error("persisted key changed across calls — second call regenerated instead of loading")
	}
}

func TestLoadOrGenerateKey_EnvOverrideWins(t *testing.T) {
	raw := make([]byte, keyByteLen)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	t.Setenv(EnvVarSecretsKey, base64.StdEncoding.EncodeToString(raw))
	dir := t.TempDir()

	k, gen, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatalf("env override: %v", err)
	}
	if gen {
		t.Error("env override should report generated=false")
	}
	if k == nil {
		t.Fatal("nil key")
	}
	// No file should be written when the env key is used.
	if _, statErr := os.Stat(filepath.Join(dir, SecretsKeyFileName)); !os.IsNotExist(statErr) {
		t.Error("env-override path must not persist a key file")
	}
}

func TestLoadOrGenerateKey_MalformedEnvFailsLoud(t *testing.T) {
	t.Setenv(EnvVarSecretsKey, "not-valid-base64-or-32-bytes")
	dir := t.TempDir()
	if _, _, err := LoadOrGenerateKey(dir); err == nil {
		t.Error("a malformed SQUADRON_SECRETS_KEY must error, not fall back to generation")
	}
}

func TestLoadOrGenerateKey_CorruptPersistedFailsLoud(t *testing.T) {
	t.Setenv(EnvVarSecretsKey, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SecretsKeyFileName), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrGenerateKey(dir); err == nil {
		t.Error("a corrupt persisted key must error rather than orphan sealed secrets by regenerating")
	}
}
