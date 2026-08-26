// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
)

// fakeAuthService is a minimal services.AuthService for the OpAMP auth tests:
// Validate returns the configured token (or error). Only Validate is exercised.
type fakeAuthService struct {
	token *services.APIToken
	err   error
}

func (f *fakeAuthService) Issue(ctx context.Context, label string, scopes []string, expiresAt *time.Time) (*services.APIToken, string, error) {
	return nil, "", nil
}
func (f *fakeAuthService) List(ctx context.Context) ([]*services.APIToken, error) { return nil, nil }
func (f *fakeAuthService) Revoke(ctx context.Context, id string) error            { return nil }
func (f *fakeAuthService) Validate(ctx context.Context, plaintext string) (*services.APIToken, error) {
	return f.token, f.err
}

func enrollToken(tenant string) *services.APIToken {
	return &services.APIToken{
		ID:       "tok-1",
		Label:    "agent-enroll",
		Scopes:   []string{services.ScopeOpAMPEnroll},
		TenantID: tenant,
	}
}

func bearerReq(t *testing.T, authz, tenantHdr string) *http.Request {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if tenantHdr != "" {
		req.Header.Set(tenantHeader, tenantHdr)
	}
	return req
}

// TestAuthenticateConn_TokenTenantWinsOverHeader is the core ADR 0042 security
// property: a valid enrollment token binds the tenant; a conflicting (spoofed)
// x-squadron-tenant header MUST NOT override it.
func TestAuthenticateConn_TokenTenantWinsOverHeader(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: enrollToken("acme")}}

	// Attacker presents a token bound to "acme" but a header claiming "victim".
	req := bearerReq(t, "Bearer sqd_valid", "victim")
	auth, accept := s.authenticateConn(context.Background(), req)

	assert.True(t, accept, "valid token is accepted")
	assert.True(t, auth.authenticated)
	assert.Equal(t, "acme", auth.tenant, "tenant comes from the token, never the header")
}

// TestAuthenticateConn_TokenEmptyTenantDefaults verifies a token with no tenant
// resolves to the OSS DefaultTenant (single-tenant inert path).
func TestAuthenticateConn_TokenEmptyTenantDefaults(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: enrollToken("")}}
	auth, accept := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_valid", ""))
	assert.True(t, accept)
	assert.True(t, auth.authenticated)
	assert.Equal(t, identity.DefaultTenant, auth.tenant)
}

// TestAuthenticateConn_GraceAcceptsUnauthenticated: with require_auth off (the
// default), a no-token connection is ACCEPTED as unauthenticated and its tenant
// falls back to the legacy header — the pilot's currently token-less agents keep
// working during the grace window.
func TestAuthenticateConn_GraceAcceptsUnauthenticated(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{}} // requireAuth=false

	// No Authorization header at all.
	auth, accept := s.authenticateConn(context.Background(), bearerReq(t, "", "tenant-a"))
	assert.True(t, accept, "grace accepts unauthenticated")
	assert.False(t, auth.authenticated)
	assert.Equal(t, "tenant-a", auth.tenant, "grace tenant falls back to the header")

	// Empty header → DefaultTenant (OSS).
	auth, accept = s.authenticateConn(context.Background(), bearerReq(t, "", ""))
	assert.True(t, accept)
	assert.Equal(t, identity.DefaultTenant, auth.tenant)
}

// TestAuthenticateConn_EnforcementRejectsUnauthenticated: with require_auth on,
// a no-token connection is REFUSED.
func TestAuthenticateConn_EnforcementRejectsUnauthenticated(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{}, requireAuth: true}
	_, accept := s.authenticateConn(context.Background(), bearerReq(t, "", "tenant-a"))
	assert.False(t, accept, "enforcement rejects an unauthenticated connection")
}

// TestAuthenticateConn_EnforcementRejectsBadToken: under enforcement, an invalid
// or insufficiently-scoped token is refused (never silently downgraded).
func TestAuthenticateConn_EnforcementRejectsBadToken(t *testing.T) {
	// Unknown token → Validate returns (nil, nil).
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: nil}, requireAuth: true}
	_, accept := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_bad", ""))
	assert.False(t, accept, "enforcement rejects an invalid token")

	// Token present but missing the opamp:enroll scope.
	noScope := &services.APIToken{ID: "t", Label: "l", Scopes: []string{services.ScopeAgentsRead}, TenantID: "acme"}
	s2 := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: noScope}, requireAuth: true}
	_, accept = s2.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_x", ""))
	assert.False(t, accept, "enforcement rejects a token lacking opamp:enroll")
}

// TestAuthenticateConn_GraceBadTokenDowngrades: under grace, a bad token is
// accepted as UNAUTHENTICATED (a misconfigured header must not break an agent
// mid-rollout) — but it does NOT authenticate, so its tenant is header-derived.
func TestAuthenticateConn_GraceBadTokenDowngrades(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: nil}} // requireAuth=false
	auth, accept := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_bad", "tenant-b"))
	assert.True(t, accept)
	assert.False(t, auth.authenticated)
	assert.Equal(t, "tenant-b", auth.tenant)
}

// TestExtractBearer covers the header parsing shared with the REST middleware.
func TestExtractBearer(t *testing.T) {
	assert.Equal(t, "sqd_abc", extractBearer("Bearer sqd_abc"))
	assert.Equal(t, "sqd_abc", extractBearer("bearer sqd_abc"))    // case-insensitive scheme
	assert.Equal(t, "sqd_abc", extractBearer("  Bearer sqd_abc ")) // trims
	assert.Equal(t, "", extractBearer(""))
	assert.Equal(t, "", extractBearer("Basic xyz")) // wrong scheme
	assert.Equal(t, "", extractBearer("sqd_no_scheme"))
}

// TestConnRateLimiter exercises the per-connection token bucket: a nil limiter
// allows everything; a real one permits the burst then blocks; and it refills
// over time.
func TestConnRateLimiter(t *testing.T) {
	var nilLimiter *connRateLimiter
	assert.True(t, nilLimiter.allow(), "nil limiter allows everything")
	assert.Nil(t, newConnRateLimiter(0), "zero rate disables rate limiting")

	// rate=10/s → burst=20. First 20 allowed, 21st blocked.
	rl := newConnRateLimiter(10)
	allowed := 0
	for i := 0; i < 20; i++ {
		if rl.allow() {
			allowed++
		}
	}
	assert.Equal(t, 20, allowed, "the full burst is permitted")
	assert.False(t, rl.allow(), "the next message is rate-limited")

	// After the bucket is drained, a refill of ~1s worth restores tokens.
	rl.mu.Lock()
	rl.last = rl.last.Add(-1 * time.Second) // simulate 1s elapsed → +10 tokens
	rl.mu.Unlock()
	assert.True(t, rl.allow(), "tokens refill over time")
}

// TestOnMessage_DropsOversizedMessage proves the ADR 0042 DoS size cap: a message
// larger than maxMessageBytes is dropped BEFORE any agent/store processing, and
// the returned ServerToAgent carries no directives.
func TestOnMessage_DropsOversizedMessage(t *testing.T) {
	s := &Server{
		logger:          zap.NewNop(),
		agents:          NewAgents(zap.NewNop()),
		maxMessageBytes: 64, // tiny cap
	}

	// A description with a large attribute value pushes proto.Size over 64 bytes.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'a'
	}
	instID := uuid.New()
	msg := &protobufs.AgentToServer{
		InstanceUid: instID[:],
		AgentDescription: &protobufs.AgentDescription{
			NonIdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("blob", string(big)),
			},
		},
	}
	resp := s.onMessage(context.Background(), nil, msg)
	assert.NotNil(t, resp)
	assert.Nil(t, resp.ConnectionSettings, "oversized message is dropped with no directives")
	// The agent registry must not have been populated (we returned before FindOrCreateAgent).
	assert.Empty(t, s.agents.GetAllAgentsReadonlyClone(), "no agent created for a dropped oversized message")
}
