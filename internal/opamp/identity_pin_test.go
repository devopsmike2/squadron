// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// fakePinResolver returns a fixed pin for any token (ADR 0052 test double).
type fakePinResolver struct{ fleetID string }

func (f fakePinResolver) PinnedFleetID(_ *services.APIToken) string { return f.fleetID }

// TestAuthenticateConn_PinPopulatedWhenResolverSet: with a pin resolver installed
// (enterprise), an authenticated connection carries the resolved pin.
func TestAuthenticateConn_PinPopulatedWhenResolverSet(t *testing.T) {
	fid := uuid.NewString()
	s := &Server{
		logger:      zap.NewNop(),
		auth:        &fakeAuthService{token: enrollToken("acme")},
		pinResolver: fakePinResolver{fleetID: fid},
	}
	auth, accept := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_valid", ""))
	assert.True(t, accept)
	assert.True(t, auth.authenticated)
	assert.Equal(t, fid, auth.pinnedFleetID, "pin resolved from the token when a resolver is set")
}

// TestAuthenticateConn_NoPinWhenNoResolver: the OSS default (no resolver) leaves
// the connection unpinned — pinning is inert in OSS.
func TestAuthenticateConn_NoPinWhenNoResolver(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: enrollToken("acme")}}
	auth, _ := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_valid", ""))
	assert.Equal(t, "", auth.pinnedFleetID, "no resolver ⇒ unpinned (OSS inert)")
}

func TestResolveFleetID(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	derived := uuid.New()
	pinned := uuid.New()

	// Unpinned context ⇒ derived unchanged (OSS path).
	if got := s.resolveFleetID(context.Background(), derived); got != derived {
		t.Errorf("unpinned: got %s, want derived %s", got, derived)
	}

	// Pin matches derived ⇒ derived.
	ctxMatch := withPinnedFleetID(context.Background(), derived.String())
	if got := s.resolveFleetID(ctxMatch, derived); got != derived {
		t.Errorf("matching pin: got %s, want %s", got, derived)
	}

	// Pin differs ⇒ PIN wins (relabel-and-log).
	ctxPin := withPinnedFleetID(context.Background(), pinned.String())
	if got := s.resolveFleetID(ctxPin, derived); got != pinned {
		t.Errorf("mismatch: got %s, want pinned %s", got, pinned)
	}

	// Malformed pin ⇒ never drop; fall back to derived.
	ctxBad := withPinnedFleetID(context.Background(), "not-a-uuid")
	if got := s.resolveFleetID(ctxBad, derived); got != derived {
		t.Errorf("malformed pin: got %s, want derived %s", got, derived)
	}
}
