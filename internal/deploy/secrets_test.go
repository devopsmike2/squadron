// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// testCrypter returns a Crypter keyed on a random 32-byte secret —
// used so tests don't need SQUADRON_DEPLOY_KEY set on the host.
func testCrypter(t *testing.T) *Crypter {
	t.Helper()
	c := &Crypter{}
	if _, err := rand.Read(c.key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return c
}

func TestCrypter_Roundtrip(t *testing.T) {
	c := testCrypter(t)
	plaintext := []byte("ghp_fake_pat_value_12345")
	sealed, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	if len(sealed) <= nonceLen {
		t.Fatalf("sealed length suspiciously small: %d", len(sealed))
	}
	got, err := c.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch: got %q want %q", got, plaintext)
	}
}

func TestCrypter_TamperingDetected(t *testing.T) {
	c := testCrypter(t)
	sealed, _ := c.Encrypt([]byte("secret"))
	// Flip a byte after the nonce.
	sealed[nonceLen] ^= 0xff
	if _, err := c.Decrypt(sealed); err == nil {
		t.Fatal("expected decrypt to fail on tampered ciphertext")
	}
}

func TestCrypter_DifferentKeyDifferentResult(t *testing.T) {
	a := testCrypter(t)
	b := testCrypter(t)
	sealed, _ := a.Encrypt([]byte("hello"))
	if _, err := b.Decrypt(sealed); err == nil {
		t.Fatal("expected decrypt with wrong key to fail")
	}
}

func TestNewCrypterFromEnv_MissingKey(t *testing.T) {
	t.Setenv(KeyEnvVar, "")
	if _, err := NewCrypterFromEnv(); err == nil {
		t.Fatal("expected ErrKeyMissing when env var unset")
	}
}

func TestNewCrypterFromEnv_BadEncoding(t *testing.T) {
	t.Setenv(KeyEnvVar, "not_base64!!!")
	if _, err := NewCrypterFromEnv(); err == nil {
		t.Fatal("expected error on malformed base64")
	}
}

func TestNewCrypterFromEnv_WrongLength(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	t.Setenv(KeyEnvVar, short)
	if _, err := NewCrypterFromEnv(); err == nil {
		t.Fatal("expected error on wrong-length key")
	}
}

func TestNewCrypterFromEnv_Valid(t *testing.T) {
	key := make([]byte, keyLen)
	_, _ = rand.Read(key)
	t.Setenv(KeyEnvVar, base64.StdEncoding.EncodeToString(key))
	c, err := NewCrypterFromEnv()
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if c == nil {
		t.Fatal("nil crypter returned with no error")
	}
}
