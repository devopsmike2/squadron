// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
)

// TestResolveConnTenant covers the ADR 0012 §Decision 2 header capture: a
// connection carrying x-squadron-tenant resolves to that tenant; an empty (or
// absent) header — the OSS single-tenant case — resolves to DefaultTenant.
func TestResolveConnTenant(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"explicit tenant", "acme", "acme"},
		{"empty header falls back to default", "", identity.DefaultTenant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set(tenantHeader, tc.header)
			}
			assert.Equal(t, tc.want, resolveConnTenant(req))
		})
	}

	// A nil request (defensive; some test paths construct no request) must
	// still resolve to DefaultTenant rather than panic.
	assert.Equal(t, identity.DefaultTenant, resolveConnTenant(nil))
}

// TestPersistAgent_StampsConnectionTenant asserts that the ctx flowing into
// persistAgent — the ctx onMessage stamps with the connection tenant — carries
// that tenant onto the store CreateAgent write (ADR 0012 §Decision 2).
func TestPersistAgent_StampsConnectionTenant(t *testing.T) {
	logger := zap.NewNop()
	mockService := new(MockAgentService)

	agentID := uuid.New()

	// New agent (GetAgent returns nil) → CreateAgent path.
	mockService.On("GetAgent", mock.Anything, agentID).Return(nil, nil)
	mockService.On("CreateAgent", mock.MatchedBy(func(ctx context.Context) bool {
		return identity.TenantFromContext(ctx) == "acme"
	}), mock.Anything).Return(nil)

	server := &Server{logger: logger, agentService: mockService}

	agent := &Agent{InstanceId: agentID, InstanceIdStr: agentID.String(), FleetId: agentID, FleetIdStr: agentID.String()}
	msg := &protobufs.AgentToServer{
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("service.name", "payments"),
			},
		},
	}

	// This mirrors what the OnMessage closure does: stamp the connection tenant.
	ctx := identity.WithTenant(context.Background(), "acme")
	server.persistAgent(ctx, agent, msg)

	mockService.AssertExpectations(t)
}

// TestPersistAgent_EmptyTenant_DefaultsToDefault asserts the OSS-inert case:
// an unstamped ctx (no x-squadron-tenant header) lands writes in DefaultTenant.
func TestPersistAgent_EmptyTenant_DefaultsToDefault(t *testing.T) {
	logger := zap.NewNop()
	mockService := new(MockAgentService)

	agentID := uuid.New()

	mockService.On("GetAgent", mock.Anything, agentID).Return(nil, nil)
	mockService.On("CreateAgent", mock.MatchedBy(func(ctx context.Context) bool {
		return identity.TenantFromContext(ctx) == identity.DefaultTenant
	}), mock.Anything).Return(nil)

	server := &Server{logger: logger, agentService: mockService}

	agent := &Agent{InstanceId: agentID, InstanceIdStr: agentID.String(), FleetId: agentID, FleetIdStr: agentID.String()}
	msg := &protobufs.AgentToServer{
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("service.name", "payments"),
			},
		},
	}

	// Simulate the empty-header path: OnConnectingFunc resolved DefaultTenant
	// and OnMessage stamped it.
	ctx := identity.WithTenant(context.Background(), identity.DefaultTenant)
	server.persistAgent(ctx, agent, msg)

	mockService.AssertExpectations(t)
}

// TestOnDisconnect_StampsConnectionTenant asserts onDisconnect stamps the
// per-connection tenant (captured at connect time) onto the offline status
// write, since the wire — and any request — is already gone (ADR 0012 §2).
func TestOnDisconnect_StampsConnectionTenant(t *testing.T) {
	logger := zap.NewNop()
	mockService := new(MockAgentService)
	agents := NewAgents(logger)

	// tracer is nil — the Tracer methods are nil-receiver-safe.
	server := &Server{logger: logger, agents: agents, agentService: mockService}

	agentID := uuid.New()
	conn := fakeTenantConn{}
	agent := &Agent{InstanceId: agentID, InstanceIdStr: agentID.String(), FleetId: agentID, FleetIdStr: agentID.String()}
	agents.agentsById[agentID] = agent
	agents.agentsByFleetId[agentID] = agent
	agents.connections[conn] = map[uuid.UUID]bool{agentID: true}

	mockService.On("UpdateAgentStatus", mock.MatchedBy(func(ctx context.Context) bool {
		return identity.TenantFromContext(ctx) == "acme"
	}), agentID, services.AgentStatusOffline).Return(nil)

	server.onDisconnect(conn, "acme")

	mockService.AssertExpectations(t)
}

// TestRawConnTenant covers the reject-decision input (ADR 0012 §Decision 2):
// the raw x-squadron-tenant header value with NO DefaultTenant fallback, so an
// absent header reads as empty ("no tenant declared") rather than "default".
func TestRawConnTenant(t *testing.T) {
	assert.Equal(t, "", rawConnTenant(nil))

	reqEmpty, _ := http.NewRequest(http.MethodGet, "/", nil)
	assert.Equal(t, "", rawConnTenant(reqEmpty))

	reqTenant, _ := http.NewRequest(http.MethodGet, "/", nil)
	reqTenant.Header.Set(tenantHeader, "acme")
	assert.Equal(t, "acme", rawConnTenant(reqTenant))
}

// shouldRejectConn replicates the OnConnectingFunc reject predicate so the seam
// decision is unit-tested without standing up a live OpAMP listener. Reject iff
// the flag is on AND no tenant is declared on the wire.
func shouldRejectConn(request *http.Request) bool {
	return rejectUntenantedConnections && rawConnTenant(request) == ""
}

// TestRejectUntenantedConnectionsSeam proves the ADR 0012 §Decision 2 contract:
//   - flag ON  + empty header   → rejected
//   - flag ON  + header present  → accepted
//   - flag OFF + empty header   → accepted (OSS inert)
func TestRejectUntenantedConnectionsSeam(t *testing.T) {
	// The OSS package default MUST be reject-off (inert).
	assert.False(t, rejectUntenantedConnections, "OSS default is reject-off")

	// Restore the default no matter what.
	defer SetRejectUntenantedConnections(false)

	reqEmpty, _ := http.NewRequest(http.MethodGet, "/", nil)
	reqTenant, _ := http.NewRequest(http.MethodGet, "/", nil)
	reqTenant.Header.Set(tenantHeader, "acme")

	// Flag ON (enterprise strict).
	SetRejectUntenantedConnections(true)
	assert.True(t, shouldRejectConn(reqEmpty), "flag on + empty header → reject")
	assert.False(t, shouldRejectConn(reqTenant), "flag on + tenant header → accept")

	// Flag OFF (OSS): never reject, even on an empty header.
	SetRejectUntenantedConnections(false)
	assert.False(t, shouldRejectConn(reqEmpty), "flag off + empty header → accept (inert)")
	assert.False(t, shouldRejectConn(reqTenant), "flag off + tenant header → accept")
}

// fakeTenantConn is a minimal comparable types.Connection used as a map key in
// the disconnect test. Its methods are never invoked on the offline path.
type fakeTenantConn struct{}

func (fakeTenantConn) Connection() net.Conn { return nil }
func (fakeTenantConn) Send(context.Context, *protobufs.ServerToAgent) error {
	return nil
}
func (fakeTenantConn) Disconnect() error { return nil }
