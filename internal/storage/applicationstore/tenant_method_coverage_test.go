// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package applicationstore

import (
	"reflect"
	"sort"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_method_coverage_test.go — the ADR 0043 "fail-closed at the test layer"
// registry. It reflects over the ENTIRE types.ApplicationStore interface and
// asserts every method is consciously classified as either tenant-SCOPED
// (routes through tenantScope, isolated per tenant — mirrors the SQLite
// reference and is exercised by the cross-tenant IDOR harness) or
// intentionally-UNSCOPED (pre-auth, system-scoped, or global-by-design).
//
// A NEWLY ADDED store method lands in NEITHER set and FAILS this test by
// default, forcing the author to decide its tenant posture — the same
// fail-closed discipline the strict registry gives the backends. If the new
// method is tenant-scoped, add it to `scoped` AND add cross-tenant coverage to
// the IDOR harness (postgres/tenant_idor_test.go + sqlite's
// tenant_scope_contract_test.go).

// scoped: every method that MUST apply a tenant predicate (its SQLite twin calls
// tenantScope). This is the cross-tenant IDOR surface.
var scoped = map[string]bool{
	"CreateAgent": true, "GetAgent": true, "ListAgents": true, "UpdateAgentStatus": true,
	"UpdateAgentLastSeen": true, "UpdateAgentEffectiveConfig": true, "UpdateAgentDeliveredConfigHash": true,
	"UpdateAgentRegistration": true, "DeleteAgent": true, "RestoreAgent": true, "PurgeAgent": true,
	"CreateGroup": true, "GetGroup": true, "ListGroups": true, "UpdateGroup": true, "DeleteGroup": true,
	"CreateConfig": true, "GetConfig": true, "ListConfigs": true, "DeleteConfig": true,
	"DeleteConfigsForAgent": true, "SetConfigArchived": true, "GetLatestConfigForAgent": true,
	"GetLatestConfigForGroup": true,
	"CreateRollout":           true, "GetRollout": true, "ListRollouts": true, "UpdateRollout": true,
	"ListAIVerdictsForGroup": true,
	"CreateSavedQuery":       true, "GetSavedQuery": true, "ListSavedQueries": true, "UpdateSavedQuery": true,
	"DeleteSavedQuery": true,
	"CreateAlertRule":  true, "GetAlertRule": true, "ListAlertRules": true, "UpdateAlertRule": true,
	"DeleteAlertRule":  true,
	"CreateAutomation": true, "GetAutomation": true, "ListAutomations": true, "UpdateAutomation": true,
	"DeleteAutomation": true,
	"CreateAuditEvent": true, "GetAuditEvent": true, "ListAuditEvents": true, "UpdateAuditEventExplanation": true,
	"ListAuditChainRows": true, "VerifyAuditChain": true,
	"CreateAPIToken": true, "ListAPITokens": true, "RevokeAPIToken": true, "UpdateAPITokenLastUsed": true,
	"UpsertExpectedAgent": true, "DeleteExpectedAgent": true, "ListExpectedAgents": true,
	"ReplaceExpectedAgentsForSource": true,
	"DismissRecommendation":          true, "RestoreRecommendation": true, "IsRecommendationDismissed": true,
	"ListRecommendationDismissals": true, "CreateRecommendationOutcome": true,
	"UpdateRecommendationOutcome": true, "ListRecommendationOutcomes": true,
	"SetRecommendationExclusion": true, "ListExcludedRecommendations": true,
	"SetCheckRunForRecommendation": true, "GetCheckRunForRecommendation": true,
	"CreateCostSpikeEvent": true, "UpdateCostSpikeEvent": true, "GetCostSpikeEvent": true,
	"ListCostSpikeEvents": true, "LatestOpenCostSpike": true,
	"CreateDeployTarget": true, "UpdateDeployTarget": true, "GetDeployTarget": true,
	"ListDeployTargets": true, "DeleteDeployTarget": true,
	"CreateDeployRun": true, "UpdateDeployRun": true, "GetDeployRun": true, "ListDeployRuns": true,
	"CreateActionRunnerRegistration": true, "UpdateActionRunnerRegistration": true,
	"GetActionRunnerRegistration": true, "ListActionRunnerRegistrations": true,
	"RevokeActionRunnerRegistration": true,
	"CreateActionRequest":            true, "UpdateActionRequest": true, "GetActionRequest": true,
	"ListActionRequests":  true,
	"CreateIncidentDraft": true, "UpdateIncidentDraft": true, "GetIncidentDraft": true,
	"GetIncidentDraftByActionRequestID": true, "ListIncidentDrafts": true,
	"SaveDiscoveryScan": true, "DeleteDiscoveryScans": true, "ListDiscoveryScans": true,
	"GetDiscoveryScan":      true,
	"ListDiscoveryVerdicts": true, "ListCrossScopeDiscoveryVerdicts": true,
	"CreateSiemDestination": true, "GetSiemDestination": true, "ListSiemDestinations": true,
	"UpdateSiemDestination": true, "DeleteSiemDestination": true, "UpdateSiemDestinationStatus": true,
	"UpsertTraceResources": true, "GetTraceResource": true, "ListTraceResourcesByScope": true,
	"CountTraceResourcesByScope": true,
}

// unscoped: methods deliberately NOT tenant-scoped, each with a reason. Their
// SQLite twins do NOT call tenantScope.
var unscoped = map[string]string{
	"GetAPITokenByHash":            "pre-auth bearer lookup, runs before any tenant is known (tenant is read off the row)",
	"Ping":                         "readiness probe: connectivity only, system context",
	"GetConnectionOwner":           "HA connection registry: instance identity is orthogonal to tenant (no tenant_id column)",
	"UpsertConnectionOwner":        "HA connection registry: system-scoped",
	"DeleteConnectionOwner":        "HA connection registry: system-scoped",
	"ListConnectionOwners":         "HA connection registry: system-scoped",
	"ReclaimStaleConnectionOwners": "HA connection registry: system-scoped sweep",
	"RecordWebhookDelivery":        "webhook delivery dedupe: global table (no tenant_id), mirrors sqlite",
	"GCWebhookDeliveries":          "webhook delivery dedupe: global retention sweep",
	"RecordRolloutApproval":        "rollout approvals: resolves tenant via the parent rollout, not the tenantScope seam (mirrors sqlite)",
	"CountRolloutApprovers":        "rollout approvals: keyed by rollout_id (mirrors sqlite)",
	"ListRolloutApprovers":         "rollout approvals: keyed by rollout_id (mirrors sqlite)",
	"WriteAuditCheckpoint":         "audit chain checkpoint: manages tenant explicitly via the checkpoint row (mirrors sqlite)",
	"ListAuditCheckpoints":         "audit chain checkpoint: manages tenant explicitly (mirrors sqlite)",
}

func TestApplicationStore_EveryMethodClassified(t *testing.T) {
	iface := reflect.TypeOf((*types.ApplicationStore)(nil)).Elem()

	var unclassified []string
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		_, isScoped := scoped[name]
		_, isUnscoped := unscoped[name]
		switch {
		case isScoped && isUnscoped:
			t.Errorf("method %q is in BOTH scoped and unscoped sets — pick one", name)
		case !isScoped && !isUnscoped:
			unclassified = append(unclassified, name)
		}
	}

	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("ADR 0043 fail-closed: %d ApplicationStore method(s) are UNCLASSIFIED for tenant scoping: %v\n"+
			"Every store method must be consciously classified. If the method is tenant-scoped, add it to `scoped` "+
			"AND add cross-tenant coverage to the IDOR harness (mirror the SQLite tenantScope). If it is "+
			"intentionally global/system/pre-auth, add it to `unscoped` with a reason.", len(unclassified), unclassified)
	}
}
