// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_idor_test.go — the ADR 0043 cross-tenant IDOR harness for the POSTGRES
// backend (the one enterprise HA actually runs on). It is the Postgres twin of
// sqlite/tenant_scope_contract_test.go, proving the two backends are now
// provably symmetric. It runs in CI's Integration job (TEST_POSTGRES_DSN set);
// testStore skips it when no Postgres is available, so the Go Backend job stays
// green without one.
//
// The core assertion for EVERY tenant-scoped, id-keyed method: write a row under
// tenant A, then call the method under tenant B — it must return not-found /
// affect zero rows / error, never A's row. Plus: strict mode fail-closes an
// unstamped context; a system context sees across tenants; and the commingling
// audit flags a deliberately-commingled fixture.

func idorAgent(id uuid.UUID) *types.Agent {
	now := time.Now().UTC()
	return &types.Agent{
		ID: id, Name: "idor-agent", Labels: map[string]string{"env": "test"},
		Status: types.AgentStatusOnline, LastSeen: now, Version: "1.0.0",
		Capabilities: []string{"metrics"}, CreatedAt: now, UpdatedAt: now,
	}
}

func idorGroup(id string) *types.Group {
	now := time.Now().UTC()
	return &types.Group{ID: id, Name: id, Labels: map[string]string{"env": "test"}, CreatedAt: now, UpdatedAt: now}
}

func idorAlertRule(id, name string) *types.AlertRule {
	return &types.AlertRule{
		ID: id, Name: name, Query: "avg(cpu)", ThresholdOperator: types.ThresholdGreater,
		ThresholdValue: 1, IntervalSeconds: 60, Severity: types.AlertSeverityWarning, Enabled: true,
	}
}

// TestPostgresTenantIDOR_Isolation is the cross-tenant IDOR proof: two stamped
// tenants; each List* returns only its own rows, and Get/Update/Delete across
// tenants is denied.
func TestPostgresTenantIDOR_Isolation(t *testing.T) {
	s := testStore(t)
	acme := identity.WithTenant(context.Background(), "acme")
	globex := identity.WithTenant(context.Background(), "globex")

	// --- agents ---
	acmeAgent, globexAgent := uuid.New(), uuid.New()
	mustNoErr(t, "create acme agent", s.CreateAgent(acme, idorAgent(acmeAgent)))
	mustNoErr(t, "create globex agent", s.CreateAgent(globex, idorAgent(globexAgent)))

	if got, err := s.ListAgents(acme); err != nil || len(got) != 1 || got[0].ID != acmeAgent {
		t.Fatalf("acme ListAgents leak: err=%v got=%v", err, got)
	}
	if cross, err := s.GetAgent(globex, acmeAgent); err != nil || cross != nil {
		t.Fatalf("IDOR: globex read acme's agent by id: got=%v err=%v", cross, err)
	}
	if err := s.UpdateAgentStatus(globex, acmeAgent, types.AgentStatusOffline); err == nil {
		t.Fatal("IDOR: globex mutated acme's agent status")
	}
	if err := s.DeleteAgent(globex, acmeAgent); err == nil {
		t.Fatal("IDOR: globex deleted acme's agent")
	}
	mustNoErr(t, "same-tenant update", s.UpdateAgentStatus(acme, acmeAgent, types.AgentStatusOffline))
	mustNoErr(t, "same-tenant delete", s.DeleteAgent(acme, acmeAgent))

	// --- groups ---
	mustNoErr(t, "create acme group", s.CreateGroup(acme, idorGroup("acme-g")))
	mustNoErr(t, "create globex group", s.CreateGroup(globex, idorGroup("globex-g")))
	if got, err := s.ListGroups(acme); err != nil || len(got) != 1 || got[0].ID != "acme-g" {
		t.Fatalf("acme ListGroups leak: err=%v got=%v", err, got)
	}
	if cross, err := s.GetGroup(globex, "acme-g"); err != nil || cross != nil {
		t.Fatalf("IDOR: globex read acme's group: got=%v err=%v", cross, err)
	}

	// --- rollouts ---
	mustNoErr(t, "create acme rollout", s.CreateRollout(acme, &types.Rollout{
		ID: "acme-ro", Name: "n", GroupID: "acme-g", TargetConfigID: "c", State: "pending"}))
	mustNoErr(t, "create globex rollout", s.CreateRollout(globex, &types.Rollout{
		ID: "globex-ro", Name: "n", GroupID: "globex-g", TargetConfigID: "c", State: "pending"}))
	if cross, err := s.GetRollout(globex, "acme-ro"); err != nil || cross != nil {
		t.Fatalf("IDOR: globex read acme's rollout: got=%v err=%v", cross, err)
	}
	if err := s.UpdateRollout(globex, &types.Rollout{
		ID: "acme-ro", Name: "n2", GroupID: "acme-g", TargetConfigID: "c", State: "paused"}); err == nil {
		t.Fatal("IDOR: globex updated acme's rollout")
	}

	// --- alert_rules (per-tenant UNIQUE(tenant_id,name)) ---
	// Distinct ids (id is the global PK), SAME name — the composite
	// UNIQUE(tenant_id,name) must let two tenants each own a "cpu" rule.
	mustNoErr(t, "acme alert rule", s.CreateAlertRule(acme, idorAlertRule("acme-cpu", "cpu")))
	mustNoErr(t, "globex alert rule same name", s.CreateAlertRule(globex, idorAlertRule("globex-cpu", "cpu")))
	if cross, err := s.GetAlertRule(globex, "acme-cpu"); err != nil || cross != nil {
		t.Fatalf("IDOR: globex read acme's alert rule by id: got=%v err=%v", cross, err)
	}
	if own, err := s.GetAlertRule(globex, "globex-cpu"); err != nil || own == nil {
		t.Fatalf("globex should see its own 'cpu' rule: got=%v err=%v", own, err)
	}

	// --- api_tokens (list scoped; GetAPITokenByHash intentionally unscoped) ---
	now := time.Now().UTC()
	mustNoErr(t, "acme token", s.CreateAPIToken(acme, &types.APIToken{ID: "acme-tok", Label: "l", Hash: "acme-h", CreatedAt: now}))
	mustNoErr(t, "globex token", s.CreateAPIToken(globex, &types.APIToken{ID: "globex-tok", Label: "l", Hash: "globex-h", CreatedAt: now}))
	if got, err := s.ListAPITokens(acme); err != nil || len(got) != 1 || got[0].ID != "acme-tok" {
		t.Fatalf("acme ListAPITokens leak: err=%v got=%v", err, got)
	}
	// The pre-auth hash lookup MUST remain unscoped (it runs before a tenant is known).
	if tok, err := s.GetAPITokenByHash(globex, "acme-h"); err != nil || tok == nil {
		t.Fatalf("GetAPITokenByHash must stay unscoped (pre-auth): got=%v err=%v", tok, err)
	}
}

// TestPostgresTenantIDOR_SystemSeesAll proves a system context crosses tenants.
func TestPostgresTenantIDOR_SystemSeesAll(t *testing.T) {
	s := testStore(t)
	acme := identity.WithTenant(context.Background(), "acme")
	globex := identity.WithTenant(context.Background(), "globex")
	system := identity.WithSystemContext(context.Background())

	mustNoErr(t, "acme agent", s.CreateAgent(acme, idorAgent(uuid.New())))
	mustNoErr(t, "globex agent", s.CreateAgent(globex, idorAgent(uuid.New())))
	if got, err := s.ListAgents(system); err != nil || len(got) != 2 {
		t.Fatalf("system ListAgents should see both tenants: err=%v len=%d", err, len(got))
	}
}

// TestPostgresTenantIDOR_StrictFailClosed proves strict mode rejects an
// unstamped context on a scoped read with ErrTenantContextRequired.
func TestPostgresTenantIDOR_StrictFailClosed(t *testing.T) {
	s := testStore(t)
	acme := identity.WithTenant(context.Background(), "acme")
	mustNoErr(t, "seed acme group", s.CreateGroup(acme, idorGroup("acme-g")))

	SetStrictTenantScoping(true)
	defer SetStrictTenantScoping(false)

	if _, err := s.ListGroups(context.Background()); !errors.Is(err, ErrTenantContextRequired) {
		t.Fatalf("unstamped read under strict must return ErrTenantContextRequired, got %v", err)
	}
	if got, err := s.ListGroups(acme); err != nil || len(got) != 1 {
		t.Fatalf("stamped read under strict should work: err=%v len=%d", err, len(got))
	}
	if got, err := s.ListGroups(identity.WithSystemContext(context.Background())); err != nil || len(got) != 1 {
		t.Fatalf("system read under strict should work: err=%v len=%d", err, len(got))
	}
}

// TestPostgresTenantIDOR_AuditDetectsCommingling seeds two tenants into one
// table and asserts the commingling audit flags it (the operator preflight).
func TestPostgresTenantIDOR_AuditDetectsCommingling(t *testing.T) {
	s := testStore(t)
	acme := identity.WithTenant(context.Background(), "acme")
	globex := identity.WithTenant(context.Background(), "globex")

	// Clean baseline: no tenants → not commingled.
	rep, err := s.AuditTenantCommingling(identity.WithSystemContext(context.Background()))
	if err != nil {
		t.Fatalf("audit (empty): %v", err)
	}
	if rep.AnyCommingled {
		t.Fatalf("empty store must not be flagged commingled: %+v", rep)
	}

	// Two distinct tenants in agents ⇒ commingled.
	mustNoErr(t, "acme agent", s.CreateAgent(acme, idorAgent(uuid.New())))
	mustNoErr(t, "globex agent", s.CreateAgent(globex, idorAgent(uuid.New())))
	rep, err = s.AuditTenantCommingling(identity.WithSystemContext(context.Background()))
	if err != nil {
		t.Fatalf("audit (commingled): %v", err)
	}
	if !rep.AnyCommingled || rep.SafeToArmStrict() {
		t.Fatalf("two tenants in agents must be flagged commingled + unsafe to arm: %+v", rep)
	}
	var agentsRow *types.TenantComminglingTableReport
	for i := range rep.Tables {
		if rep.Tables[i].Table == "agents" {
			agentsRow = &rep.Tables[i]
		}
	}
	if agentsRow == nil || agentsRow.DistinctTenants != 2 || !agentsRow.Commingled {
		t.Fatalf("agents row should show 2 distinct tenants + commingled: %+v", agentsRow)
	}
}

func mustNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
