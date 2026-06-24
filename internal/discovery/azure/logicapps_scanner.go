// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Orchestration tier slice 1 chunk 3 constants — see
// docs/proposals/orchestration-tier-slice1.md §3.3.
const (
	// LogicAppsAppInsightsConnString — app_setting key whose
	// non-empty value flips Standard-tier HasTraceAxis. Mirrors the
	// Functions scanner's AppInsightsConnectionStringAppSetting.
	LogicAppsAppInsightsConnString = "APPLICATIONINSIGHTS_CONNECTION_STRING"

	// LogicAppsWorkflowAppKindPrefix — Microsoft.Web/sites kind
	// prefix identifying a Standard-tier Logic App. Matches
	// "workflowapp" / "workflowapp,linux". The hybrid kind
	// "functionapp,workflowapp" is rejected (Functions territory) to
	// avoid double-counting.
	LogicAppsWorkflowAppKindPrefix = "workflowapp"

	// API versions — Consumption-tier list, Standard-tier list
	// (shared with the Functions scanner), and the Insights
	// diagnostic-settings sub-resource (shared with the SQL scanner).
	LogicAppsWorkflowsAPIVersion          = "2019-05-01"
	LogicAppsWorkflowAppKindAPIVersion    = armWebAppListAPIVersion
	LogicAppsDiagnosticSettingsAPIVersion = armDiagSettingsAPIVersion

	// ServiceIDLogicApps — per-service identifier accumulated under
	// Result.FailedServices for BOTH tiers.
	ServiceIDLogicApps = "logicapps"

	// logicAppsOrchestrationSurface — drives proposer recommendation
	// routing (logicapps-* → Azure).
	logicAppsOrchestrationSurface = "logicapps"

	// WorkflowType disjunction values. Standard uses App Service
	// (app_settings detection); Consumption uses the multi-tenant
	// managed runtime (diagnostic-settings detection). Both flow
	// through OrchestrationInstanceSnapshot.
	LogicAppsWorkflowTypeStandard    = "Standard"
	LogicAppsWorkflowTypeConsumption = "Consumption"
)

// ScanOrchestrations is the Azure scanner's orchestration-tier entry
// point and satisfies handlers.OrchestrationDiscoveryScanner. Slice 1
// chunk 3 covers Logic Apps only — both Standard tier
// (Microsoft.Web/sites?kind=workflowapp*) and Consumption tier
// (Microsoft.Logic/workflows + diagnostic settings).
//
// Mirrors the AWS Step Functions scanner posture: standalone method,
// dispatched separately from Scan()'s per-region loop. The two tiers
// run sequentially under one OAuth token; a failure on either records
// a partial under "logicapps" and the other tier still produces rows.
// Snapshots from BOTH tiers return in one slice; the per-row
// WorkflowType discriminates Standard vs Consumption.
//
// Scope semantics: scope.Regions is ignored (Logic Apps surfaces are
// subscription-scope). scope.AccountID overrides the per-row
// AccountID; empty falls back to s.SubscriptionID.
//
// IAM: Reader at subscription scope (already required by slice 1 VM
// walk) covers all three API surfaces. No additional role.
func (s *Scanner) ScanOrchestrations(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	token, err := s.acquireAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure: %s: %w", ServiceIDLogicApps, err)
	}
	result := &scanner.Result{}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.SubscriptionID
	}
	result.AccountID = accountID
	s.ScanLogicApps(ctx, token, result)
	return result.Orchestrations, nil
}

// ScanLogicApps runs the two-tier walk and appends
// OrchestrationInstanceSnapshot entries to result.Orchestrations.
//
// Pass 1 (Standard tier) — Microsoft.Web/sites?kind=workflowapp*:
// for each site POST .../config/appsettings/list; presence of
// APPLICATIONINSIGHTS_CONNECTION_STRING flips HasTraceAxis.
//
// Pass 2 (Consumption tier) — Microsoft.Logic/workflows: for each
// workflow GET microsoft.insights/diagnosticSettings; any setting
// present flips HasLogAxis, an App Insights destination (workspaceId
// or applicationInsights.connectionString) flips HasTraceAxis. 404
// is the canonical "no settings" shape (both axes false; no partial).
func (s *Scanner) ScanLogicApps(ctx context.Context, accessToken string, result *scanner.Result) {
	s.scanLogicAppsStandard(ctx, accessToken, result)
	s.scanLogicAppsConsumption(ctx, accessToken, result)
}

// scanLogicAppsStandard walks the Standard tier (App Service hosted).
// Per-site app_settings failures still surface the row with
// HasTraceAxis=false so the inventory remains visible (matches the
// Functions scanner's partial-failure posture).
func (s *Scanner) scanLogicAppsStandard(ctx context.Context, accessToken string, result *scanner.Result) {
	sites, listErr := s.listLogicAppsWorkflowApps(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDLogicApps, classifyLogicAppsError(listErr))
		return
	}
	if len(sites) == 0 {
		return
	}
	for _, site := range sites {
		rg := parseRGFromARMID(site.ID)
		if rg == "" {
			recordPartialFailure(result, ServiceIDLogicApps,
				fmt.Sprintf("%s: workflowapp %s missing resource group in ARM id", ServiceIDLogicApps, site.Name))
			continue
		}
		settings, settingsErr := s.listFunctionAppSettings(ctx, accessToken, rg, site.Name)
		if settingsErr != nil {
			recordPartialFailure(result, ServiceIDLogicApps, classifyLogicAppsError(settingsErr))
			result.Orchestrations = append(result.Orchestrations,
				projectLogicAppsStandardNoSettings(site, result.AccountID))
			continue
		}
		result.Orchestrations = append(result.Orchestrations,
			projectLogicAppsStandard(site, settings, result.AccountID))
	}
}

// scanLogicAppsConsumption walks the Consumption tier
// (Microsoft.Logic/workflows). Per-workflow diagnostic-settings
// failures still surface the row with both axes false; a 404 is the
// canonical "no settings" shape (no partial).
func (s *Scanner) scanLogicAppsConsumption(ctx context.Context, accessToken string, result *scanner.Result) {
	workflows, listErr := s.listLogicAppsWorkflows(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDLogicApps, classifyLogicAppsError(listErr))
		return
	}
	if len(workflows) == 0 {
		return
	}
	for _, wf := range workflows {
		hasTrace, hasLog, diagErr := s.probeLogicAppsDiagnostics(ctx, accessToken, wf.ID)
		if diagErr != nil {
			recordPartialFailure(result, ServiceIDLogicApps, classifyLogicAppsError(diagErr))
			result.Orchestrations = append(result.Orchestrations,
				projectLogicAppsConsumption(wf, false, false, result.AccountID))
			continue
		}
		result.Orchestrations = append(result.Orchestrations,
			projectLogicAppsConsumption(wf, hasTrace, hasLog, result.AccountID))
	}
}

// listLogicAppsWorkflowApps walks Microsoft.Web/sites, follows
// nextLink, and filters to workflowapp-prefix kinds client-side
// (rationale documented on listFunctionApps: $filter coverage is
// inconsistent and comma-suffixed kinds would miss).
func (s *Scanner) listLogicAppsWorkflowApps(ctx context.Context, accessToken string) ([]armWebSite, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Web/sites?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		LogicAppsWorkflowAppKindAPIVersion,
	)

	var out []armWebSite
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armWebSiteListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("logic apps standard list parse: %w", jerr)}
		}
		for _, site := range page.Value {
			if isLogicAppsWorkflowApp(site) {
				out = append(out, site)
			}
		}
		pageURL = page.NextLink
	}
	return out, nil
}

// isLogicAppsWorkflowApp reports whether a Microsoft.Web/sites entry
// is a Standard-tier Logic App. Matches "workflowapp" /
// "workflowapp,linux"; the hybrid "functionapp,workflowapp" kind is
// Functions territory and rejected to avoid double-counting.
func isLogicAppsWorkflowApp(site armWebSite) bool {
	return strings.HasPrefix(site.Kind, LogicAppsWorkflowAppKindPrefix)
}

// listLogicAppsWorkflows walks Microsoft.Logic/workflows
// subscription-scope and follows nextLink. Only Consumption-tier
// Logic Apps surface here.
func (s *Scanner) listLogicAppsWorkflows(ctx context.Context, accessToken string) ([]armLogicWorkflow, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Logic/workflows?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		LogicAppsWorkflowsAPIVersion,
	)

	var out []armLogicWorkflow
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armLogicWorkflowListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("logic apps consumption list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// probeLogicAppsDiagnostics returns (hasTrace, hasLog, err) for a
// workflow's diagnostic-settings. hasLog flips when ANY setting is
// present (§3.3). hasTrace flips when workspaceId or
// applicationInsights.connectionString is populated (App Insights
// routing). 404 → (false, false, nil) — canonical "no settings".
func (s *Scanner) probeLogicAppsDiagnostics(ctx context.Context, accessToken, workflowARMID string) (hasTrace, hasLog bool, err error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(workflowARMID, "/")
	diagURL := fmt.Sprintf(
		"%s/%s/providers/microsoft.insights/diagnosticSettings?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		resourceID,
		LogicAppsDiagnosticSettingsAPIVersion,
	)

	body, callErr := s.doARMGet(ctx, accessToken, diagURL)
	if callErr != nil {
		var ace *armCallError
		if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
			return false, false, nil
		}
		return false, false, callErr
	}

	var resp armLogicDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, false, &armCallError{Wrapped: fmt.Errorf("logic apps diagnostic settings parse: %w", jerr)}
	}
	for _, ds := range resp.Value {
		// Any setting present satisfies the §3.3 "if any diagnostic
		// setting is configured AT ALL" rule.
		hasLog = true
		// Trace axis: workspaceId (Log Analytics → App Insights via
		// continuous export, per §3.3) OR
		// applicationInsights.connectionString flips the bit.
		if ds.Properties.WorkspaceID != "" || ds.Properties.ApplicationInsights.ConnectionString != "" {
			hasTrace = true
		}
	}
	return hasTrace, hasLog, nil
}

// newLogicAppsSnapshot builds the common envelope shared across the
// Standard and Consumption projections. Per-tier helpers then layer
// on the axis fields and Detail bag.
func newLogicAppsSnapshot(name, arn, region, accountID, workflowType string) scanner.OrchestrationInstanceSnapshot {
	return scanner.OrchestrationInstanceSnapshot{
		Provider:     azureProviderID,
		Surface:      logicAppsOrchestrationSurface,
		AccountID:    accountID,
		Region:       region,
		ResourceName: name,
		ResourceARN:  arn,
		WorkflowType: workflowType,
	}
}

// projectLogicAppsStandard maps a (site, app_settings) pair into an
// OrchestrationInstanceSnapshot. Standard-tier rule: HasTraceAxis ←
// APPLICATIONINSIGHTS_CONNECTION_STRING non-empty. HasLogAxis stays
// false (no load-bearing log-axis signal on app_settings; slice 2 may
// add a Log Analytics workspace-id probe).
func projectLogicAppsStandard(site armWebSite, settings map[string]string, accountID string) scanner.OrchestrationInstanceSnapshot {
	snap := newLogicAppsSnapshot(site.Name, site.ID, site.Location, accountID, LogicAppsWorkflowTypeStandard)
	if v, ok := settings[LogicAppsAppInsightsConnString]; ok && v != "" {
		snap.HasTraceAxis = true
	}
	snap.Detail = map[string]any{
		"kind":             site.Kind,
		"workflow_type":    LogicAppsWorkflowTypeStandard,
		"has_app_insights": snap.HasTraceAxis,
	}
	return snap
}

// projectLogicAppsStandardNoSettings is the partial-failure helper:
// surfaces the row with HasTraceAxis=false when the app_settings call
// failed so the operator still sees the inventory.
func projectLogicAppsStandardNoSettings(site armWebSite, accountID string) scanner.OrchestrationInstanceSnapshot {
	snap := newLogicAppsSnapshot(site.Name, site.ID, site.Location, accountID, LogicAppsWorkflowTypeStandard)
	snap.Detail = map[string]any{
		"kind":            site.Kind,
		"workflow_type":   LogicAppsWorkflowTypeStandard,
		"settings_unread": true,
	}
	return snap
}

// projectLogicAppsConsumption maps a (workflow, hasTrace, hasLog)
// triple into an OrchestrationInstanceSnapshot. Consumption-tier axes
// flow through verbatim from probeLogicAppsDiagnostics.
func projectLogicAppsConsumption(wf armLogicWorkflow, hasTrace, hasLog bool, accountID string) scanner.OrchestrationInstanceSnapshot {
	snap := newLogicAppsSnapshot(wf.Name, wf.ID, wf.Location, accountID, LogicAppsWorkflowTypeConsumption)
	snap.HasTraceAxis = hasTrace
	snap.HasLogAxis = hasLog
	snap.Detail = map[string]any{
		"workflow_type": LogicAppsWorkflowTypeConsumption,
		"state":         wf.Properties.State,
		"has_trace":     hasTrace,
		"has_log":       hasLog,
	}
	return snap
}

// classifyLogicAppsError maps a walk failure into the operator-visible
// PartialReason string under "logicapps". Mirrors classifyARMError /
// classifyAzureSQLError / classifyAzureFunctionsError shapes.
func classifyLogicAppsError(err error) string {
	if err == nil {
		return ""
	}
	var ace *armCallError
	if errors.As(err, &ace) {
		if ace.IsNetwork {
			wrapped := ""
			if ace.Wrapped != nil {
				wrapped = ace.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", ServiceIDLogicApps, truncate(wrapped, 200))
		}
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDLogicApps)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", ServiceIDLogicApps)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", ServiceIDLogicApps)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", ServiceIDLogicApps)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: Logic Apps walk failed (HTTP %d): %s", ServiceIDLogicApps, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDLogicApps, truncate(err.Error(), 200))
}
