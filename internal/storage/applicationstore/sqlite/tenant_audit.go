// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/tenantaudit"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenantAuditTables is every tenant-bearing SQLite table the ADR 0043
// commingling audit scans. It is the Postgres set plus the discovery-observation
// tables that only the SQLite backend carries. Missing tables are skipped by the
// shared scanner, so listing a table a given database never created is harmless.
var tenantAuditTables = []tenantaudit.Table{
	{Name: "agents", IDCol: "id"},
	{Name: "groups", IDCol: "id"},
	{Name: "configs", IDCol: "id"},
	{Name: "rollouts", IDCol: "id"},
	{Name: "saved_queries", IDCol: "id"},
	{Name: "alert_rules", IDCol: "id"},
	{Name: "automations", IDCol: "id"},
	{Name: "audit_events", IDCol: "id"},
	{Name: "deploy_targets", IDCol: "id"},
	{Name: "deploy_runs", IDCol: "id"},
	{Name: "api_tokens", IDCol: "id"},
	{Name: "recommendation_outcomes", IDCol: "id"},
	{Name: "cost_spike_events", IDCol: "id"},
	{Name: "action_requests", IDCol: "id"},
	{Name: "incident_drafts", IDCol: "id"},
	{Name: "discovery_scans", IDCol: "scan_id"},
	{Name: "siem_destinations", IDCol: "id"},
	{Name: "expected_agents", IDCol: "hostname"},
	{Name: "recommendation_dismissals", IDCol: "recommendation_id"},
	{Name: "action_runner_registrations", IDCol: "runner_id"},
	{Name: "iac_recommendation_verdicts", IDCol: "recommendation_id"},
	{Name: "trace_resource_seen", IDCol: "resource_key"},
	// SQLite-only discovery-observation tables.
	{Name: "serverless_instance", IDCol: "id"},
	{Name: "orchestration_instance", IDCol: "id"},
	{Name: "event_source_instance", IDCol: "id"},
	{Name: "cold_start_observation", IDCol: "id"},
	{Name: "error_rate_observation", IDCol: "id"},
}

// AuditTenantCommingling implements types.TenantComminglingAuditor: the ADR 0043
// read-only preflight an operator runs before arming strict tenant scoping. It
// issues NO tenant predicate and MUTATES NOTHING.
func (s *Storage) AuditTenantCommingling(ctx context.Context) (*types.TenantComminglingReport, error) {
	return tenantaudit.Run(ctx, s.db, "sqlite", tenantAuditTables)
}
