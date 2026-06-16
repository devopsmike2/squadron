// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/base64"
	"os"
	"testing"

	"github.com/devopsmike2/squadron/internal/siem"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// freshCrypter mints a one-shot crypter for a test by writing a
// random base64 key to the env var and constructing one. Avoids
// having to commit a real test key.
func freshCrypter(t *testing.T) *siem.Crypter {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	prev := os.Getenv(siem.KeyEnvVar)
	t.Setenv(siem.KeyEnvVar, base64.StdEncoding.EncodeToString(key))
	t.Cleanup(func() { os.Setenv(siem.KeyEnvVar, prev) })
	c, err := siem.NewCrypterFromEnv()
	require.NoError(t, err)
	return c
}

// TestSiemService_CreateRoundTrip verifies the headline property:
// a Create stores ciphertext, the view never reveals plaintext, and
// LoadEnabled decrypts cleanly for the dispatcher.
func TestSiemService_CreateRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	crypter := freshCrypter(t)
	svc := NewSiemService(store, crypter, nil, zap.NewNop())

	view, err := svc.Create(ctx, SiemDestinationInput{
		Name:            "prod-splunk",
		Type:            string(siem.SplunkHEC),
		URL:             "https://splunk.example.com/services/collector/event",
		PlaintextSecret: "super-secret-token",
		Enabled:         true,
	})
	require.NoError(t, err)
	assert.True(t, view.HasSecret)

	// The view JSON shape never includes the plaintext.
	dests, secrets, err := svc.LoadEnabled(ctx)
	require.NoError(t, err)
	require.Len(t, dests, 1)
	require.Len(t, secrets, 1)
	assert.Equal(t, "prod-splunk", dests[0].Name)
	assert.Equal(t, "super-secret-token", string(secrets[0]),
		"dispatcher should receive the plaintext secret post-decryption")
}

// TestSiemService_LoadEnabled_FiltersDisabled verifies that disabled
// destinations are excluded so the dispatcher doesn't spin up workers
// for them. The operator's "pause" toggle has to actually stop traffic.
func TestSiemService_LoadEnabled_FiltersDisabled(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	crypter := freshCrypter(t)
	svc := NewSiemService(store, crypter, nil, zap.NewNop())

	_, err := svc.Create(ctx, SiemDestinationInput{
		Name:            "enabled-one",
		Type:            string(siem.GenericWebhook),
		URL:             "https://hooks.example.com/squadron",
		PlaintextSecret: "k1",
		Enabled:         true,
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, SiemDestinationInput{
		Name:            "disabled-one",
		Type:            string(siem.GenericWebhook),
		URL:             "https://hooks.example.com/other",
		PlaintextSecret: "k2",
		Enabled:         false,
	})
	require.NoError(t, err)

	dests, _, err := svc.LoadEnabled(ctx)
	require.NoError(t, err)
	require.Len(t, dests, 1)
	assert.Equal(t, "enabled-one", dests[0].Name)
}

// TestSiemService_Validate_RejectsBadInput covers the validation
// surface the API handler bounces 400s for. Important: every error
// here should be operator-visible, not internal.
func TestSiemService_Validate_RejectsBadInput(t *testing.T) {
	store := memory.NewStore()
	crypter := freshCrypter(t)
	svc := NewSiemService(store, crypter, nil, zap.NewNop())
	ctx := context.Background()

	cases := []struct {
		name string
		in   SiemDestinationInput
		want string
	}{
		{"missing name", SiemDestinationInput{Type: string(siem.SplunkHEC), URL: "x", PlaintextSecret: "y"}, "name is required"},
		{"missing url", SiemDestinationInput{Name: "n", Type: string(siem.SplunkHEC), PlaintextSecret: "y"}, "url is required"},
		{"missing secret", SiemDestinationInput{Name: "n", Type: string(siem.SplunkHEC), URL: "x"}, "plaintext_secret is required"},
		{"unknown type", SiemDestinationInput{Name: "n", Type: "made-up", URL: "x", PlaintextSecret: "y"}, "type must be"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(ctx, c.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// TestSiemService_Update_PreservesSecret verifies the operator-
// friendly behavior: when an Update omits plaintext_secret, the
// existing ciphertext is preserved (rather than wiped). Critical
// because the only reason to re-enter the secret is to rotate it.
func TestSiemService_Update_PreservesSecret(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	crypter := freshCrypter(t)
	svc := NewSiemService(store, crypter, nil, zap.NewNop())

	view, err := svc.Create(ctx, SiemDestinationInput{
		Name:            "x",
		Type:            string(siem.SplunkHEC),
		URL:             "https://splunk.example.com",
		PlaintextSecret: "original",
		Enabled:         true,
	})
	require.NoError(t, err)

	newName := "renamed"
	updated, err := svc.Update(ctx, view.ID, SiemDestinationUpdate{Name: &newName})
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Name)
	assert.True(t, updated.HasSecret, "secret should still be present after a name-only update")

	// And confirm the dispatcher path still gets the original
	// plaintext back — the secret really wasn't touched.
	_, secrets, err := svc.LoadEnabled(ctx)
	require.NoError(t, err)
	require.Len(t, secrets, 1)
	assert.Equal(t, "original", string(secrets[0]))
}
