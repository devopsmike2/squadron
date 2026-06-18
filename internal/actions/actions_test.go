// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawParams is a tiny helper that builds a json.RawMessage from
// any struct without forcing every test to inline json.Marshal.
func rawParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestSignerRoundTrip verifies that a request signed by Signer
// passes Verifier.Verify and that the public key serialization
// formats survive a round trip.
func TestSignerRoundTrip(t *testing.T) {
	s, err := GenerateSigner()
	require.NoError(t, err)
	v, err := NewVerifier(s.PublicKey())
	require.NoError(t, err)

	req := &Request{
		RequestID:  "req-1",
		ProposalID: "prop-1",
		RunnerID:   "runner-1",
		Action: ActionPayload{
			Type:       RestartSystemdServiceType,
			Parameters: rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx"}),
		},
		Phase: PhaseDryRun,
	}
	sig, err := s.Sign(req)
	require.NoError(t, err)
	assert.NotEmpty(t, sig)
	assert.False(t, req.IssuedAt.IsZero(), "Sign should default IssuedAt")
	assert.False(t, req.ExpiresAt.IsZero(), "Sign should default ExpiresAt")
	assert.WithinDuration(t, req.IssuedAt.Add(5*time.Minute), req.ExpiresAt, time.Second)

	// Verify within validity window
	require.NoError(t, v.Verify(req, req.IssuedAt.Add(30*time.Second)))
}

// TestSignerExpired ensures a captured request becomes useless
// after its expiry.
func TestSignerExpired(t *testing.T) {
	s, _ := GenerateSigner()
	v, _ := NewVerifier(s.PublicKey())
	req := &Request{
		RequestID: "req-x",
		RunnerID:  "r",
		Action:    ActionPayload{Type: RestartSystemdServiceType, Parameters: rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx"})},
		Phase:     PhaseDryRun,
	}
	require.NoError(t, mustSign(s, req))
	err := v.Verify(req, req.ExpiresAt.Add(time.Second))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
}

// TestSignerTamperedPayload ensures any change to a signed request
// after signing invalidates the signature.
func TestSignerTamperedPayload(t *testing.T) {
	s, _ := GenerateSigner()
	v, _ := NewVerifier(s.PublicKey())
	req := &Request{
		RequestID: "req-x",
		RunnerID:  "r",
		Action:    ActionPayload{Type: RestartSystemdServiceType, Parameters: rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx"})},
		Phase:     PhaseDryRun,
	}
	require.NoError(t, mustSign(s, req))
	// Swap parameters out from under the signature.
	req.Action.Parameters = rawParams(t, RestartSystemdServiceParameters{UnitName: "rootkit"})
	err := v.Verify(req, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signature verification failed")
}

// TestSignerWrongKey ensures a signature from a different signer
// is rejected even when the rest of the request is structurally
// valid.
func TestSignerWrongKey(t *testing.T) {
	good, _ := GenerateSigner()
	bad, _ := GenerateSigner()
	v, _ := NewVerifier(good.PublicKey())
	req := &Request{RequestID: "r1", RunnerID: "r", Action: ActionPayload{Type: RestartSystemdServiceType, Parameters: rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx"})}, Phase: PhaseDryRun}
	require.NoError(t, mustSign(bad, req))
	err := v.Verify(req, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signature verification failed")
}

// TestSignerPEMRoundTrip ensures the PEM-encoded public key parses
// back into a Verifier that can verify a real request.
func TestSignerPEMRoundTrip(t *testing.T) {
	s, _ := GenerateSigner()
	pemStr := s.PublicKeyPEM()
	assert.Contains(t, pemStr, "BEGIN ED25519 PUBLIC KEY")
	v, err := NewVerifierFromPEM([]byte(pemStr))
	require.NoError(t, err)
	req := &Request{RequestID: "r1", RunnerID: "r", Action: ActionPayload{Type: RestartSystemdServiceType, Parameters: rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx"})}, Phase: PhaseDryRun}
	require.NoError(t, mustSign(s, req))
	require.NoError(t, v.Verify(req, time.Now()))
}

// TestFingerprintStable verifies the fingerprint is stable across
// calls and short enough to put in a log line.
func TestFingerprintStable(t *testing.T) {
	s, _ := GenerateSigner()
	fp := s.Fingerprint()
	assert.Equal(t, fp, s.Fingerprint(), "fingerprint should be deterministic for a key")
	assert.Less(t, len(fp), 32, "fingerprint should be short enough for log lines")
}

// TestRegistryRegisterAndGet covers the basic registry lifecycle.
func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(RestartSystemdServiceActionType()))
	at, ok := r.Get(RestartSystemdServiceType)
	require.True(t, ok)
	assert.Equal(t, RestartSystemdServiceType, at.Type)

	_, ok = r.Get("unknown")
	assert.False(t, ok)

	err := r.Register(RestartSystemdServiceActionType())
	assert.Error(t, err, "duplicate registration must error")
}

// TestRegistryRejectsBadTypes catches the obvious shape errors.
func TestRegistryRejectsBadTypes(t *testing.T) {
	r := NewRegistry()
	assert.Error(t, r.Register(ActionType{}), "empty Type")
	assert.Error(t, r.Register(ActionType{Type: "x"}), "missing ValidateParameters")
	assert.Error(t, r.Register(ActionType{Type: "x", ValidateParameters: func(json.RawMessage) error { return nil }}), "missing MatchesCapability")
}

// TestDefaultRegistryHasRestartSystemd verifies the init() in
// register_restart_systemd.go ran and the type is discoverable.
func TestDefaultRegistryHasRestartSystemd(t *testing.T) {
	at, ok := Default.Get(RestartSystemdServiceType)
	require.True(t, ok)
	assert.Equal(t, RestartSystemdServiceType, at.Type)
	assert.Contains(t, Default.Types(), RestartSystemdServiceType)
}

// TestValidateRestartSystemdParameters covers the parameter
// schema validator.
func TestValidateRestartSystemdParameters(t *testing.T) {
	ok := func(p RestartSystemdServiceParameters) {
		assert.NoError(t, validateRestartSystemdParameters(rawParams(t, p)))
	}
	bad := func(p RestartSystemdServiceParameters, contains string) {
		err := validateRestartSystemdParameters(rawParams(t, p))
		require.Error(t, err)
		assert.Contains(t, err.Error(), contains)
	}
	ok(RestartSystemdServiceParameters{UnitName: "nginx"})
	ok(RestartSystemdServiceParameters{UnitName: "nginx", RestartStrategy: "try-restart"})
	ok(RestartSystemdServiceParameters{UnitName: "nginx", RestartStrategy: ""}) // default
	bad(RestartSystemdServiceParameters{}, "unit_name is required")
	bad(RestartSystemdServiceParameters{UnitName: "../etc/passwd"}, "path separators")
	bad(RestartSystemdServiceParameters{UnitName: "nginx", RestartStrategy: "rm-rf"}, "not one of")
}

// TestAllowsAction_HappyPath verifies capability matching when a
// runner declared a glob that fits the proposed unit name.
func TestAllowsAction_HappyPath(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(RestartSystemdServiceActionType()))
	caps := []Capability{{
		Type: RestartSystemdServiceType,
		Constraints: map[string]any{
			"unit_name_glob": []any{"squadron-*", "nginx*"},
		},
	}}
	params := rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx-frontend"})
	allowed, _ := r.AllowsAction(caps, RestartSystemdServiceType, params)
	assert.True(t, allowed)
}

// TestAllowsAction_OutOfPolicy verifies refusal when the proposed
// unit name falls outside the runner's declared globs.
func TestAllowsAction_OutOfPolicy(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(RestartSystemdServiceActionType()))
	caps := []Capability{{
		Type:        RestartSystemdServiceType,
		Constraints: map[string]any{"unit_name_glob": []any{"squadron-*"}},
	}}
	params := rawParams(t, RestartSystemdServiceParameters{UnitName: "sshd"})
	allowed, reason := r.AllowsAction(caps, RestartSystemdServiceType, params)
	assert.False(t, allowed)
	assert.Contains(t, reason, "sshd")
	assert.Contains(t, reason, "does not match any glob")
}

// TestAllowsAction_UnknownType verifies the registry rejects
// action types that haven't been registered, before any capability
// check fires.
func TestAllowsAction_UnknownType(t *testing.T) {
	r := NewRegistry()
	allowed, reason := r.AllowsAction(nil, "definitely-not-a-real-action", nil)
	assert.False(t, allowed)
	assert.Contains(t, reason, "unknown action type")
}

// TestAllowsAction_NoCapability verifies refusal when the runner
// hasn't declared the requested type at all.
func TestAllowsAction_NoCapability(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(RestartSystemdServiceActionType()))
	caps := []Capability{{Type: "something-else"}}
	params := rawParams(t, RestartSystemdServiceParameters{UnitName: "nginx"})
	allowed, reason := r.AllowsAction(caps, RestartSystemdServiceType, params)
	assert.False(t, allowed)
	assert.Contains(t, reason, "no capability declaration")
}

// TestAllowsAction_EmptyGlobAllowsAny documents the runner-config
// shorthand: declaring the capability with no constraint means
// "any unit." The operator who installed the runner opted into
// that explicitly.
func TestAllowsAction_EmptyGlobAllowsAny(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(RestartSystemdServiceActionType()))
	caps := []Capability{{Type: RestartSystemdServiceType}}
	params := rawParams(t, RestartSystemdServiceParameters{UnitName: "whatever"})
	allowed, _ := r.AllowsAction(caps, RestartSystemdServiceType, params)
	assert.True(t, allowed)
}

// mustSign is a tiny helper used in negative tests where we
// don't care about the signature string itself, just that signing
// succeeded.
func mustSign(s *Signer, r *Request) error {
	_, err := s.Sign(r)
	return err
}
