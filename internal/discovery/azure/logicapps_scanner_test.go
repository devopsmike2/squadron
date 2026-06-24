// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// fakeAzureLogicApps routes the four endpoints the orchestration-tier
// slice 1 chunk 3 walker exercises: token, Microsoft.Web/sites (for
// Standard-tier kind filtering), Microsoft.Logic/workflows (for
// Consumption-tier listing), the per-Standard app_settings POST, and
// the per-Consumption diagnostic-settings GET. Parallel to
// fakeAzureFunctions / fakeAzureSQL / fakeAzureAKS.

type fakeAzureLogicApps struct {
	mu sync.Mutex

	Sites                  []armWebSite
	Workflows              []armLogicWorkflow
	SettingsBySite         map[string]map[string]string
	DiagSettingsByWorkflow map[string]armLogicDiagnosticSettingsResponse

	// Pagination — when set, the list endpoint serves these pages.
	SitesPages     []armWebSiteListResponse
	WorkflowsPages []armLogicWorkflowListResponse

	// Failure-injection knobs.
	SitesListStatus     int
	WorkflowsListStatus int
	AppSettingsStatus   int
	DiagSettingsStatus  int

	// Call counters for assertions.
	TokenCalls         int
	SitesListCalls     int
	WorkflowsListCalls int
	AppSettingsCalls   int
	DiagSettingsCalls  int
	LastBearer         string
}

func newFakeAzureLogicApps() *fakeAzureLogicApps {
	return &fakeAzureLogicApps{
		SettingsBySite:         map[string]map[string]string{},
		DiagSettingsByWorkflow: map[string]armLogicDiagnosticSettingsResponse{},
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeStatus(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(armErrorResponse{
		Error: armErrorBody{Code: armErrorCodeFor(status), Message: msg},
	})
}

func (f *fakeAzureLogicApps) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			f.TokenCalls++
			writeJSON(w, armTokenResponse{AccessToken: "fake-bearer-token", TokenType: "Bearer", ExpiresIn: 3600})

		case strings.HasSuffix(path, "/providers/Microsoft.Web/sites"):
			f.SitesListCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.SitesListStatus != 0 {
				writeStatus(w, f.SitesListStatus, "sites list failure")
				return
			}
			if len(f.SitesPages) > 0 {
				idx := f.SitesListCalls - 1
				if idx >= len(f.SitesPages) {
					idx = len(f.SitesPages) - 1
				}
				writeJSON(w, f.SitesPages[idx])
				return
			}
			writeJSON(w, armWebSiteListResponse{Value: f.Sites})

		case strings.HasSuffix(path, "/providers/Microsoft.Logic/workflows"):
			f.WorkflowsListCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.WorkflowsListStatus != 0 {
				writeStatus(w, f.WorkflowsListStatus, "workflows list failure")
				return
			}
			if len(f.WorkflowsPages) > 0 {
				idx := f.WorkflowsListCalls - 1
				if idx >= len(f.WorkflowsPages) {
					idx = len(f.WorkflowsPages) - 1
				}
				writeJSON(w, f.WorkflowsPages[idx])
				return
			}
			writeJSON(w, armLogicWorkflowListResponse{Value: f.Workflows})

		case strings.Contains(path, "/providers/Microsoft.Web/sites/") && strings.HasSuffix(path, "/config/appsettings/list"):
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			f.AppSettingsCalls++
			if f.AppSettingsStatus != 0 {
				writeStatus(w, f.AppSettingsStatus, "app settings failure")
				return
			}
			writeJSON(w, armAppSettingsResponse{
				Properties: f.SettingsBySite[extractSiteNameFromAppSettingsPath(path)],
			})

		case strings.Contains(path, "/providers/Microsoft.Logic/workflows/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			f.DiagSettingsCalls++
			if f.DiagSettingsStatus != 0 {
				writeStatus(w, f.DiagSettingsStatus, "diag failure")
				return
			}
			wfName := extractWorkflowNameFromDiagPath(path)
			settings, ok := f.DiagSettingsByWorkflow[wfName]
			if !ok {
				writeStatus(w, http.StatusNotFound, "no diagnostic settings")
				return
			}
			writeJSON(w, settings)

		default:
			writeStatus(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path))
		}
	})
}

func newLogicAppsScannerWithFake(t *testing.T, fake *fakeAzureLogicApps) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}
}

// extractWorkflowNameFromDiagPath pulls the {workflow} segment out of
// the per-workflow diagnostic-settings URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Logic/workflows/<workflow>/providers/microsoft.insights/diagnosticSettings
func extractWorkflowNameFromDiagPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "workflows" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// --- Helpers ---------------------------------------------------------

const testSubID = "22222222-2222-2222-2222-222222222222"

func makeWorkflowApp(name, location, kind string) armWebSite {
	return armWebSite{
		ID:       fmt.Sprintf("/subscriptions/%s/resourceGroups/rg-%s/providers/Microsoft.Web/sites/%s", testSubID, name, name),
		Name:     name,
		Location: location,
		Kind:     kind,
	}
}

func makeLogicWorkflow(name, location, state string) armLogicWorkflow {
	return armLogicWorkflow{
		ID:         fmt.Sprintf("/subscriptions/%s/resourceGroups/rg-%s/providers/Microsoft.Logic/workflows/%s", testSubID, name, name),
		Name:       name,
		Location:   location,
		Properties: armLogicWorkflowProperties{State: state},
	}
}

func diagSettings(name string, props armLogicDiagnosticSettingProperties) armLogicDiagnosticSettingsResponse {
	return armLogicDiagnosticSettingsResponse{
		Value: []armLogicDiagnosticSetting{{Name: name, Properties: props}},
	}
}

func diagSettingsWithAppInsights(connStr string) armLogicDiagnosticSettingsResponse {
	return diagSettings("ds-ai", armLogicDiagnosticSettingProperties{
		ApplicationInsights: armLogicDiagnosticAppInsightsDestination{ConnectionString: connStr},
	})
}

func diagSettingsWithWorkspace(workspaceID string) armLogicDiagnosticSettingsResponse {
	return diagSettings("ds-la", armLogicDiagnosticSettingProperties{WorkspaceID: workspaceID})
}

func diagSettingsBare() armLogicDiagnosticSettingsResponse {
	return diagSettings("ds-bare", armLogicDiagnosticSettingProperties{
		StorageAccountID: "/subscriptions/x/y/providers/Microsoft.Storage/storageAccounts/sa1",
	})
}

// --- Tests -----------------------------------------------------------

// Acceptance test 7 — Standard tier with App Insights connection string.
func TestLogicAppsScanner_StandardWithAppInsights_HasTraceAxis(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{
		makeWorkflowApp("la-std-1", "eastus", "workflowapp"),
	}
	fake.SettingsBySite["la-std-1"] = map[string]string{
		LogicAppsAppInsightsConnString: "InstrumentationKey=abc;IngestionEndpoint=https://x",
	}

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasTraceAxis, "Standard tier with APPLICATIONINSIGHTS_CONNECTION_STRING must flip HasTraceAxis")
	assert.Equal(t, LogicAppsWorkflowTypeStandard, snap.WorkflowType)
	assert.Equal(t, logicAppsOrchestrationSurface, snap.Surface)
	assert.Equal(t, azureProviderID, snap.Provider)
	assert.Equal(t, "la-std-1", snap.ResourceName)
	assert.Equal(t, "eastus", snap.Region)
}

func TestLogicAppsScanner_StandardWithoutAppInsights_NoTraceAxis(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{
		makeWorkflowApp("la-std-bare", "eastus", "workflowapp"),
	}
	fake.SettingsBySite["la-std-bare"] = map[string]string{
		"OTHER_KEY": "value",
	}

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.False(t, snaps[0].HasTraceAxis)
	assert.False(t, snaps[0].HasLogAxis)
	assert.Equal(t, LogicAppsWorkflowTypeStandard, snaps[0].WorkflowType)
}

// Acceptance test 8 — Consumption tier with diagnostic settings flips axes.
func TestLogicAppsScanner_ConsumptionWithDiagnosticSettings_HasTraceAxis(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Workflows = []armLogicWorkflow{
		makeLogicWorkflow("la-cons-1", "westus", "Enabled"),
	}
	fake.DiagSettingsByWorkflow["la-cons-1"] = diagSettingsWithAppInsights(
		"InstrumentationKey=abc;IngestionEndpoint=https://x")

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasTraceAxis, "Consumption tier with App Insights diag setting must flip HasTraceAxis")
	assert.True(t, snap.HasLogAxis, "Any diagnostic setting present must flip HasLogAxis")
	assert.Equal(t, LogicAppsWorkflowTypeConsumption, snap.WorkflowType)
	assert.Equal(t, "la-cons-1", snap.ResourceName)
	assert.Equal(t, "westus", snap.Region)
}

// Acceptance test 9 — Consumption tier without diagnostic settings (404).
func TestLogicAppsScanner_ConsumptionWithoutDiagnosticSettings_NoTraceAxis(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Workflows = []armLogicWorkflow{
		makeLogicWorkflow("la-cons-bare", "westus", "Enabled"),
	}
	// Intentionally not seeding DiagSettingsByWorkflow — the fake
	// returns 404, which the scanner treats as "no settings".

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.False(t, snaps[0].HasTraceAxis)
	assert.False(t, snaps[0].HasLogAxis)
	assert.Equal(t, LogicAppsWorkflowTypeConsumption, snaps[0].WorkflowType)
}

func TestLogicAppsScanner_BothTiersInOneScan_PopulatesBoth(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{
		makeWorkflowApp("la-std-mixed", "eastus", "workflowapp,linux"),
		// Also include a Web App that should NOT appear in the
		// orchestration result.
		{ID: "/subscriptions/x/y/Microsoft.Web/sites/web1", Name: "web1", Location: "eastus", Kind: "app"},
	}
	fake.SettingsBySite["la-std-mixed"] = map[string]string{
		LogicAppsAppInsightsConnString: "connstr",
	}
	fake.Workflows = []armLogicWorkflow{
		makeLogicWorkflow("la-cons-mixed", "westus", "Enabled"),
	}
	fake.DiagSettingsByWorkflow["la-cons-mixed"] = diagSettingsWithWorkspace(
		"/subscriptions/x/y/Microsoft.OperationalInsights/workspaces/w1")

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 2)

	byType := map[string]scanner.OrchestrationInstanceSnapshot{}
	for _, snap := range snaps {
		byType[snap.WorkflowType] = snap
	}
	stdSnap, hasStd := byType[LogicAppsWorkflowTypeStandard]
	consSnap, hasCons := byType[LogicAppsWorkflowTypeConsumption]
	require.True(t, hasStd, "Standard tier snapshot must be present")
	require.True(t, hasCons, "Consumption tier snapshot must be present")
	assert.True(t, stdSnap.HasTraceAxis)
	assert.True(t, consSnap.HasTraceAxis, "workspaceId destination must flip HasTraceAxis")
	assert.True(t, consSnap.HasLogAxis)
}

func TestLogicAppsScanner_KindFilter_OnlyIncludesWorkflowApps(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{
		makeWorkflowApp("la-std", "eastus", "workflowapp"),
		{ID: "/subscriptions/x/resourceGroups/rg/providers/Microsoft.Web/sites/fn1", Name: "fn1", Location: "eastus", Kind: "functionapp"},
		{ID: "/subscriptions/x/resourceGroups/rg/providers/Microsoft.Web/sites/web1", Name: "web1", Location: "eastus", Kind: "app"},
		// Hybrid kind — Functions territory; orchestration walker
		// must NOT include it.
		{ID: "/subscriptions/x/resourceGroups/rg/providers/Microsoft.Web/sites/fn-wf", Name: "fn-wf", Location: "eastus", Kind: "functionapp,workflowapp"},
	}
	fake.SettingsBySite["la-std"] = map[string]string{
		LogicAppsAppInsightsConnString: "connstr",
	}

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1, "only the canonical workflowapp kind must be reported")
	assert.Equal(t, "la-std", snaps[0].ResourceName)
}

// newPaginatedScanner builds a Scanner against a freshly-spun fake
// server so a test can mutate the fake.*Pages slices' NextLink fields
// to embed the live server URL after construction.
func newPaginatedScanner(t *testing.T, fake *fakeAzureLogicApps) (*Scanner, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}, srv
}

func TestLogicAppsScanner_PaginationFollowsNextLink_StandardTier(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.SitesPages = []armWebSiteListResponse{
		{Value: []armWebSite{makeWorkflowApp("la-page1", "eastus", "workflowapp")}},
		{Value: []armWebSite{makeWorkflowApp("la-page2", "eastus", "workflowapp")}},
	}
	fake.SettingsBySite["la-page1"] = map[string]string{LogicAppsAppInsightsConnString: "c1"}
	fake.SettingsBySite["la-page2"] = map[string]string{LogicAppsAppInsightsConnString: "c2"}

	s, srv := newPaginatedScanner(t, fake)
	fake.SitesPages[0].NextLink = fmt.Sprintf(
		"%s/subscriptions/22222222-2222-2222-2222-222222222222/providers/Microsoft.Web/sites?api-version=%s&page=2",
		srv.URL, LogicAppsWorkflowAppKindAPIVersion,
	)

	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	assert.Equal(t, 2, fake.SitesListCalls, "expected two page calls for sites list")
}

func TestLogicAppsScanner_PaginationFollowsNextLink_ConsumptionTier(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.WorkflowsPages = []armLogicWorkflowListResponse{
		{Value: []armLogicWorkflow{makeLogicWorkflow("la-wp1", "eastus", "Enabled")}},
		{Value: []armLogicWorkflow{makeLogicWorkflow("la-wp2", "westus", "Enabled")}},
	}

	s, srv := newPaginatedScanner(t, fake)
	fake.WorkflowsPages[0].NextLink = fmt.Sprintf(
		"%s/subscriptions/22222222-2222-2222-2222-222222222222/providers/Microsoft.Logic/workflows?api-version=%s&page=2",
		srv.URL, LogicAppsWorkflowsAPIVersion,
	)

	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	assert.Equal(t, 2, fake.WorkflowsListCalls, "expected two page calls for workflows list")
}

func TestLogicAppsScanner_StandardTier_WorkflowTypeFieldIsStandard(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{makeWorkflowApp("la-typestd", "eastus", "workflowapp,linux")}
	fake.SettingsBySite["la-typestd"] = map[string]string{LogicAppsAppInsightsConnString: "c"}

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "Standard", snaps[0].WorkflowType,
		"Standard-tier rows must carry WorkflowType=\"Standard\"")
	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, "Standard", snaps[0].Detail["workflow_type"])
}

func TestLogicAppsScanner_ConsumptionTier_WorkflowTypeFieldIsConsumption(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Workflows = []armLogicWorkflow{makeLogicWorkflow("la-typecons", "eastus", "Enabled")}

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "Consumption", snaps[0].WorkflowType,
		"Consumption-tier rows must carry WorkflowType=\"Consumption\"")
	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, "Consumption", snaps[0].Detail["workflow_type"])
}

// --- Additional surgical coverage ----------------------------------

func TestLogicAppsScanner_AppSettingsFailure_StillSurfacesStandardRow(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{makeWorkflowApp("la-std", "eastus", "workflowapp")}
	fake.AppSettingsStatus = http.StatusForbidden

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1, "Standard row must still surface when app_settings fails")
	assert.False(t, snaps[0].HasTraceAxis)
	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, true, snaps[0].Detail["settings_unread"])
}

func TestLogicAppsScanner_AccountIDOverrideAndFallback(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Sites = []armWebSite{makeWorkflowApp("la", "eastus", "workflowapp")}
	fake.SettingsBySite["la"] = map[string]string{LogicAppsAppInsightsConnString: "c"}

	s := newLogicAppsScannerWithFake(t, fake)
	// Fallback.
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", snaps[0].AccountID)
	// Override.
	snaps2, err2 := s.ScanOrchestrations(context.Background(), scanner.ScanScope{AccountID: "override"})
	require.NoError(t, err2)
	require.Len(t, snaps2, 1)
	assert.Equal(t, "override", snaps2[0].AccountID)
}

func TestLogicAppsScanner_BareDiagSetting_HasLogAxisOnly(t *testing.T) {
	fake := newFakeAzureLogicApps()
	fake.Workflows = []armLogicWorkflow{makeLogicWorkflow("la-bare", "eastus", "Enabled")}
	fake.DiagSettingsByWorkflow["la-bare"] = diagSettingsBare()

	s := newLogicAppsScannerWithFake(t, fake)
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.True(t, snaps[0].HasLogAxis, "any diag setting present flips HasLogAxis")
	assert.False(t, snaps[0].HasTraceAxis, "storage-only destination does not flip HasTraceAxis")
}

func TestIsLogicAppsWorkflowApp_HybridFunctionAppRejected(t *testing.T) {
	assert.False(t, isLogicAppsWorkflowApp(armWebSite{Kind: "functionapp,workflowapp"}),
		"hybrid kind must not match — Functions territory")
	assert.True(t, isLogicAppsWorkflowApp(armWebSite{Kind: "workflowapp"}))
	assert.True(t, isLogicAppsWorkflowApp(armWebSite{Kind: "workflowapp,linux"}))
	assert.False(t, isLogicAppsWorkflowApp(armWebSite{Kind: "app"}))
	assert.False(t, isLogicAppsWorkflowApp(armWebSite{Kind: "functionapp"}))
}

func TestClassifyLogicAppsError_StatusMatrix(t *testing.T) {
	got403 := classifyLogicAppsError(&armCallError{StatusCode: http.StatusForbidden})
	assert.Contains(t, got403, "permission denied")
	assert.Contains(t, got403, ServiceIDLogicApps)
	got429 := classifyLogicAppsError(&armCallError{StatusCode: http.StatusTooManyRequests})
	assert.Contains(t, strings.ToLower(got429), "rate limit")
}
