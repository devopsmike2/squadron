// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// tenantCapturingAudit records the tenant carried by the ctx passed to
// Record, so a webhook test can assert the store write scoped to the
// connection's tenant (ADR 0012 §Decision 3).
type tenantCapturingAudit struct {
	mu         sync.Mutex
	tenants    []string
	systemSeen []bool
}

func (a *tenantCapturingAudit) Record(ctx context.Context, _ services.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tenants = append(a.tenants, identity.TenantFromContext(ctx))
	a.systemSeen = append(a.systemSeen, identity.IsSystemContext(ctx))
	return nil
}

func (a *tenantCapturingAudit) List(context.Context, services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return nil, nil
}

func (a *tenantCapturingAudit) Get(context.Context, string) (*services.AuditEvent, error) {
	return nil, nil
}

func (a *tenantCapturingAudit) SetExplanation(context.Context, string, string, string, time.Time) error {
	return nil
}

// seedConnectionWithTenant inserts a connection carrying the given
// tenant and returns its ConnectionID.
func seedConnectionWithTenant(t *testing.T, store iacconnstore.Store, repoFullName, tenant string) string {
	t.Helper()
	conn := &iacconnstore.IaCConnection{
		Provider:       iacconnstore.ProviderGitHub,
		AuthKind:       iacconnstore.AuthKindPAT,
		RepoFullName:   repoFullName,
		DefaultBranch:  "main",
		RepoLayout:     iacconnstore.RepoLayoutMono,
		CredCiphertext: []byte("opaque-test-blob"),
		TenantID:       tenant,
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	return conn.ConnectionID
}

// TestWebhook_MatchedConnection_StampsConnTenant asserts ADR 0012
// §Decision 3: a delivery matching a connection stamps that
// connection's tenant onto the store write ctx (here observed via the
// audit Record ctx).
func TestWebhook_MatchedConnection_StampsConnTenant(t *testing.T) {
	audit := &tenantCapturingAudit{}
	store := iacconnstore.NewMemoryStore()
	h := NewIaCGitHubWebhookHandler(audit, store, webhookTestSecret, zap.NewNop())
	seedConnectionWithTenant(t, store, "octo/widgets", "acme")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/eks-observability-addon/abc123",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.tenants) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.tenants))
	}
	if audit.tenants[0] != "acme" {
		t.Errorf("audit ctx tenant = %q, want %q", audit.tenants[0], "acme")
	}
	if audit.systemSeen[0] {
		t.Errorf("audit ctx should NOT be system context for a matched connection")
	}
}

// TestWebhook_MatchedConnection_EmptyTenant_DefaultsToDefault asserts
// the OSS-inert path: a connection with no tenant (default) stamps
// "default" onto the store write ctx.
func TestWebhook_MatchedConnection_EmptyTenant_DefaultsToDefault(t *testing.T) {
	audit := &tenantCapturingAudit{}
	store := iacconnstore.NewMemoryStore()
	h := NewIaCGitHubWebhookHandler(audit, store, webhookTestSecret, zap.NewNop())
	// seedConnection (no explicit tenant) → Create defaults to "default".
	seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/eks-observability-addon/abc123",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.tenants) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.tenants))
	}
	if audit.tenants[0] != identity.DefaultTenant {
		t.Errorf("audit ctx tenant = %q, want %q", audit.tenants[0], identity.DefaultTenant)
	}
}

// TestWebhook_UnmatchedRepo_StampsSystemContext asserts ADR 0012
// §Decision 3: an HMAC-authed delivery whose repo matches no connection
// stamps the SYSTEM context onto the store write ctx, so the dedupe /
// audit row is recorded fleet-wide.
func TestWebhook_UnmatchedRepo_StampsSystemContext(t *testing.T) {
	audit := &tenantCapturingAudit{}
	store := iacconnstore.NewMemoryStore()
	h := NewIaCGitHubWebhookHandler(audit, store, webhookTestSecret, zap.NewNop())
	// No connection seeded for this repo → connectionID stays empty.

	body := makePREventBody(t, "closed", true, "octo/unmatched", 7,
		"squadron/rec/eks-observability-addon/abc123",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.tenants) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.tenants))
	}
	if !audit.systemSeen[0] {
		t.Errorf("audit ctx should be system context for an unmatched delivery; tenant=%q", audit.tenants[0])
	}
}

// VerifyChain — ADR 0027 slice 1. Test stub: self-tenant audit chain
// verify. Not exercised by these tests; returns a trivially OK result.
func (a *tenantCapturingAudit) VerifyChain(context.Context) (*applicationstore.AuditChainVerification, error) {
	return &applicationstore.AuditChainVerification{OK: true}, nil
}
