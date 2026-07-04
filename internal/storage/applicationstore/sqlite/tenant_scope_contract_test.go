// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_scope_contract_test.go — the editions-contract for ADR 0011 M2.
//
// Group 1 proves the scoping is INERT in the OSS build: with an unstamped
// context (strict off — the OSS default) every representative table
// round-trips and List* returns all rows, exactly as it would with no
// tenant logic. Group 2 proves real isolation once tenants are stamped.
// Group 3 proves a system context sees across tenants. Group 4 proves the
// strict flag fails fast on an unstamped context.

// makeAlertRuleTenant builds a valid alert rule for the contract tests.
func makeAlertRuleTenant(id string) *types.AlertRule {
	return &types.AlertRule{
		ID:                id,
		Name:              id, // name is UNIQUE (single-column) — keep it per-id
		Query:             "avg(cpu)",
		ThresholdOperator: types.ThresholdGreater,
		ThresholdValue:    1,
		IntervalSeconds:   60,
		Severity:          types.AlertSeverityWarning,
		Enabled:           true,
	}
}

// TestTenantScope_OSSByteIdentical is Group 1: with an unstamped context
// (strict OFF), a representative set round-trips and List* returns all
// rows — the predicate is a no-op when everything is 'default'.
func TestTenantScope_OSSByteIdentical(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background() // unstamped: the OSS inert path
		require.False(t, strictTenantScoping, "OSS default is strict OFF")

		// agents
		agentID := uuid.New()
		require.NoError(t, store.CreateAgent(ctx, makeTestAgent(agentID)))
		gotAgent, err := store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, gotAgent)
		agents, err := store.ListAgents(ctx)
		require.NoError(t, err)
		require.Len(t, agents, 1)

		// groups
		require.NoError(t, store.CreateGroup(ctx, makeTestGroup("g1")))
		gotGroup, err := store.GetGroup(ctx, "g1")
		require.NoError(t, err)
		require.NotNil(t, gotGroup)
		groups, err := store.ListGroups(ctx)
		require.NoError(t, err)
		require.Len(t, groups, 1)

		// configs
		gid := "g1"
		require.NoError(t, store.CreateConfig(ctx, makeTestConfig("c1", nil, &gid)))
		gotConfig, err := store.GetConfig(ctx, "c1")
		require.NoError(t, err)
		require.NotNil(t, gotConfig)
		configs, err := store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		require.Len(t, configs, 1)

		// rollouts
		require.NoError(t, store.CreateRollout(ctx, &types.Rollout{
			ID: "ro1", Name: "n", GroupID: "g1", TargetConfigID: "c1", State: "pending"}))
		gotRollout, err := store.GetRollout(ctx, "ro1")
		require.NoError(t, err)
		require.NotNil(t, gotRollout)
		rollouts, err := store.ListRollouts(ctx, types.RolloutFilter{})
		require.NoError(t, err)
		require.Len(t, rollouts, 1)

		// alert_rules
		require.NoError(t, store.CreateAlertRule(ctx, makeAlertRuleTenant("ar1")))
		gotRule, err := store.GetAlertRule(ctx, "ar1")
		require.NoError(t, err)
		require.NotNil(t, gotRule)
		rules, err := store.ListAlertRules(ctx)
		require.NoError(t, err)
		require.Len(t, rules, 1)

		// audit_events
		require.NoError(t, store.CreateAuditEvent(ctx, &types.AuditEvent{
			ID: "ae1", Timestamp: time.Now().UTC(), Actor: "op",
			EventType: "rollout.created", TargetType: "rollout", Action: "created"}))
		events, err := store.ListAuditEvents(ctx, types.AuditEventFilter{})
		require.NoError(t, err)
		require.Len(t, events, 1)

		// api_tokens
		require.NoError(t, store.CreateAPIToken(ctx, &types.APIToken{
			ID: "tok1", Label: "l", Hash: "h1", CreatedAt: time.Now().UTC()}))
		tokens, err := store.ListAPITokens(ctx)
		require.NoError(t, err)
		require.Len(t, tokens, 1)
	})
}

// TestTenantScope_Isolation is Group 2: two stamped tenants; each List*
// returns only its own rows, and Get/Update/Delete across tenants is
// denied.
func TestTenantScope_Isolation(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		acme := identity.WithTenant(context.Background(), "acme")
		globex := identity.WithTenant(context.Background(), "globex")

		// --- agents ---
		acmeAgent := uuid.New()
		globexAgent := uuid.New()
		require.NoError(t, store.CreateAgent(acme, makeTestAgent(acmeAgent)))
		require.NoError(t, store.CreateAgent(globex, makeTestAgent(globexAgent)))

		acmeAgents, err := store.ListAgents(acme)
		require.NoError(t, err)
		require.Len(t, acmeAgents, 1)
		require.Equal(t, acmeAgent, acmeAgents[0].ID)

		globexAgents, err := store.ListAgents(globex)
		require.NoError(t, err)
		require.Len(t, globexAgents, 1)
		require.Equal(t, globexAgent, globexAgents[0].ID)

		// cross-tenant Get returns not-found
		cross, err := store.GetAgent(globex, acmeAgent)
		require.NoError(t, err)
		require.Nil(t, cross, "globex must not see acme's agent by id")

		// cross-tenant Update affects 0 rows (returns not-found error)
		require.Error(t, store.UpdateAgentStatus(globex, acmeAgent, types.AgentStatusOffline),
			"globex must not mutate acme's agent")
		// same-tenant Update works
		require.NoError(t, store.UpdateAgentStatus(acme, acmeAgent, types.AgentStatusOffline))

		// cross-tenant Delete affects 0 rows
		require.Error(t, store.DeleteAgent(globex, acmeAgent), "globex must not delete acme's agent")
		require.NoError(t, store.DeleteAgent(acme, acmeAgent))

		// --- groups ---
		require.NoError(t, store.CreateGroup(acme, makeTestGroup("acme-g")))
		require.NoError(t, store.CreateGroup(globex, makeTestGroup("globex-g")))
		acmeGroups, err := store.ListGroups(acme)
		require.NoError(t, err)
		require.Len(t, acmeGroups, 1)
		require.Equal(t, "acme-g", acmeGroups[0].ID)
		crossGroup, err := store.GetGroup(globex, "acme-g")
		require.NoError(t, err)
		require.Nil(t, crossGroup)

		// --- rollouts ---
		require.NoError(t, store.CreateRollout(acme, &types.Rollout{
			ID: "acme-ro", Name: "n", GroupID: "acme-g", TargetConfigID: "c", State: "pending"}))
		require.NoError(t, store.CreateRollout(globex, &types.Rollout{
			ID: "globex-ro", Name: "n", GroupID: "globex-g", TargetConfigID: "c", State: "pending"}))
		acmeRollouts, err := store.ListRollouts(acme, types.RolloutFilter{})
		require.NoError(t, err)
		require.Len(t, acmeRollouts, 1)
		crossRo, err := store.GetRollout(globex, "acme-ro")
		require.NoError(t, err)
		require.Nil(t, crossRo)
		// cross-tenant UpdateRollout is not-found
		require.Error(t, store.UpdateRollout(globex, &types.Rollout{
			ID: "acme-ro", Name: "n2", GroupID: "acme-g", TargetConfigID: "c", State: "paused"}))

		// --- audit_events ---
		require.NoError(t, store.CreateAuditEvent(acme, &types.AuditEvent{
			ID: "acme-ae", Timestamp: time.Now().UTC(), Actor: "op",
			EventType: "rollout.created", TargetType: "rollout", Action: "created"}))
		require.NoError(t, store.CreateAuditEvent(globex, &types.AuditEvent{
			ID: "globex-ae", Timestamp: time.Now().UTC(), Actor: "op",
			EventType: "rollout.created", TargetType: "rollout", Action: "created"}))
		acmeEvents, err := store.ListAuditEvents(acme, types.AuditEventFilter{})
		require.NoError(t, err)
		require.Len(t, acmeEvents, 1)
		require.Equal(t, "acme-ae", acmeEvents[0].ID)

		// --- api_tokens ---
		require.NoError(t, store.CreateAPIToken(acme, &types.APIToken{
			ID: "acme-tok", Label: "l", Hash: "acme-h", CreatedAt: time.Now().UTC()}))
		require.NoError(t, store.CreateAPIToken(globex, &types.APIToken{
			ID: "globex-tok", Label: "l", Hash: "globex-h", CreatedAt: time.Now().UTC()}))
		acmeTokens, err := store.ListAPITokens(acme)
		require.NoError(t, err)
		require.Len(t, acmeTokens, 1)
		require.Equal(t, "acme-tok", acmeTokens[0].ID)

		t.Logf("ISOLATION OK: acme sees %d agent(s)/%d group(s)/%d rollout(s)/%d audit(s)/%d token(s); globex sees %d agent(s)",
			len(acmeAgents), len(acmeGroups), len(acmeRollouts), len(acmeEvents), len(acmeTokens), len(globexAgents))
	})
}

// TestTenantScope_SystemSeesAll is Group 3: a system context sees both
// tenants' rows.
func TestTenantScope_SystemSeesAll(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		acme := identity.WithTenant(context.Background(), "acme")
		globex := identity.WithTenant(context.Background(), "globex")
		system := identity.WithSystemContext(context.Background())

		require.NoError(t, store.CreateAgent(acme, makeTestAgent(uuid.New())))
		require.NoError(t, store.CreateAgent(globex, makeTestAgent(uuid.New())))
		require.NoError(t, store.CreateGroup(acme, makeTestGroup("acme-g")))
		require.NoError(t, store.CreateGroup(globex, makeTestGroup("globex-g")))

		agents, err := store.ListAgents(system)
		require.NoError(t, err)
		require.Len(t, agents, 2, "system context sees both tenants' agents")

		groups, err := store.ListGroups(system)
		require.NoError(t, err)
		require.Len(t, groups, 2, "system context sees both tenants' groups")
	})
}

// TestTenantScope_StrictFlag is Group 4: with strict scoping on, an
// unstamped context on a scoped read errors; a system or stamped context
// works. Strict is reset in a defer so sibling tests are unaffected.
func TestTenantScope_StrictFlag(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)

		// Seed a row under a stamped tenant while strict is still off.
		acme := identity.WithTenant(context.Background(), "acme")
		require.NoError(t, store.CreateGroup(acme, makeTestGroup("acme-g")))

		SetStrictTenantScoping(true)
		defer SetStrictTenantScoping(false)

		// Unstamped read fails fast.
		_, err := store.ListGroups(context.Background())
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrTenantContextRequired),
			"unstamped read under strict scoping must return ErrTenantContextRequired")

		// Stamped read works.
		groups, err := store.ListGroups(acme)
		require.NoError(t, err)
		require.Len(t, groups, 1)

		// System read works.
		sys, err := store.ListGroups(identity.WithSystemContext(context.Background()))
		require.NoError(t, err)
		require.Len(t, sys, 1)
	})
}
