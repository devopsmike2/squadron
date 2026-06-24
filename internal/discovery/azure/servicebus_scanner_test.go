// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
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

// fakeAzureServiceBus routes the three endpoints the event-source-tier
// slice 1 chunk 3 walker exercises: token, Microsoft.ServiceBus/
// namespaces list, and the per-namespace microsoft.insights/
// diagnosticSettings GET. Parallel to fakeAzureLogicApps (v0.89.96).
type fakeAzureServiceBus struct {
	mu sync.Mutex

	Namespaces             []armServiceBusNamespace
	DiagSettingsByNS       map[string]armServiceBusDiagnosticSettingsResponse
	NamespacesPages        []armServiceBusNamespaceListResponse

	// Failure-injection knobs.
	NamespacesListStatus int
	DiagSettingsStatus   int
	DiagSettings404ForNS map[string]bool

	// Call counters for assertions.
	TokenCalls         int
	NamespacesCalls    int
	DiagSettingsCalls  int
	LastBearer         string
}

func newFakeAzureServiceBus() *fakeAzureServiceBus {
	return &fakeAzureServiceBus{
		DiagSettingsByNS:     map[string]armServiceBusDiagnosticSettingsResponse{},
		DiagSettings404ForNS: map[string]bool{},
	}
}

func (f *fakeAzureServiceBus) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			f.TokenCalls++
			writeJSON(w, armTokenResponse{
				AccessToken: "fake-bearer-token",
				TokenType:   "Bearer",
				ExpiresIn:   3600,
			})

		case strings.HasSuffix(path, "/providers/Microsoft.ServiceBus/namespaces"):
			f.NamespacesCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.NamespacesListStatus != 0 {
				writeStatus(w, f.NamespacesListStatus, "namespaces list failure")
				return
			}
			if len(f.NamespacesPages) > 0 {
				idx := f.NamespacesCalls - 1
				if idx >= len(f.NamespacesPages) {
					idx = len(f.NamespacesPages) - 1
				}
				writeJSON(w, f.NamespacesPages[idx])
				return
			}
			writeJSON(w, armServiceBusNamespaceListResponse{Value: f.Namespaces})

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			f.DiagSettingsCalls++
			nsName := extractNamespaceFromDiagPath(path)
			if f.DiagSettings404ForNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no diagnostic settings")
				return
			}
			if f.DiagSettingsStatus != 0 {
				writeStatus(w, f.DiagSettingsStatus, "diag failure")
				return
			}
			settings, ok := f.DiagSettingsByNS[nsName]
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

// extractNamespaceFromDiagPath pulls the {namespace} segment out of
// the per-namespace diagnostic-settings URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.ServiceBus/namespaces/<namespace>/providers/microsoft.insights/diagnosticSettings
func extractNamespaceFromDiagPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "namespaces" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func newServiceBusScannerWithFake(t *testing.T, fake *fakeAzureServiceBus) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: testSubID,
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}
}

func newPaginatedServiceBusScanner(t *testing.T, fake *fakeAzureServiceBus) (*Scanner, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: testSubID,
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}, srv
}

// --- Helpers ----------------------------------------------------------

func makeServiceBusNamespace(name, location, sku string) armServiceBusNamespace {
	return armServiceBusNamespace{
		ID: fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/rg-%s/providers/Microsoft.ServiceBus/namespaces/%s",
			testSubID, name, name,
		),
		Name:     name,
		Location: location,
		Type:     "Microsoft.ServiceBus/Namespaces",
		SKU:      armServiceBusNamespaceSKU{Name: sku, Tier: sku},
	}
}

func sbDiagWithAppInsights(connStr string) armServiceBusDiagnosticSettingsResponse {
	return armServiceBusDiagnosticSettingsResponse{
		Value: []armServiceBusDiagnosticSetting{{
			Name: "ds-ai",
			Properties: armServiceBusDiagnosticSettingProperties{
				ApplicationInsights: armServiceBusDiagnosticAppInsightsDestination{
					ConnectionString: connStr,
				},
			},
		}},
	}
}

func sbDiagWithWorkspace(workspaceID string) armServiceBusDiagnosticSettingsResponse {
	return armServiceBusDiagnosticSettingsResponse{
		Value: []armServiceBusDiagnosticSetting{{
			Name: "ds-la",
			Properties: armServiceBusDiagnosticSettingProperties{
				WorkspaceID: workspaceID,
			},
		}},
	}
}

func sbDiagWithEventHub(authRuleID string) armServiceBusDiagnosticSettingsResponse {
	return armServiceBusDiagnosticSettingsResponse{
		Value: []armServiceBusDiagnosticSetting{{
			Name: "ds-eh",
			Properties: armServiceBusDiagnosticSettingProperties{
				EventHubAuthRuleID: authRuleID,
				EventHubName:       "eh-1",
			},
		}},
	}
}

// --- Tests -----------------------------------------------------------

// Acceptance test 7 — namespace with App Insights diagnostic settings.
func TestServiceBusScanner_NamespaceWithAppInsightsDiagnostics_HasTraceAxis(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-ai", "eastus", "Standard"),
	}
	fake.DiagSettingsByNS["sb-ai"] = sbDiagWithAppInsights(
		"InstrumentationKey=abc;IngestionEndpoint=https://x")

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasTraceAxis, "App Insights diagnostic setting must flip HasTraceAxis")
	assert.True(t, snap.HasLogAxis, "any diagnostic setting present flips HasLogAxis")
	assert.Equal(t, serviceBusEventSourceSurface, snap.Surface)
	assert.Equal(t, azureProviderID, snap.Provider)
	assert.Equal(t, serviceBusSourceTypeNamespace, snap.SourceType)
}

// Acceptance test 8 — namespace with Log Analytics workspace diagnostic settings.
func TestServiceBusScanner_NamespaceWithLogAnalyticsDiagnostics_HasTraceAxis(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-la", "westus", "Premium"),
	}
	fake.DiagSettingsByNS["sb-la"] = sbDiagWithWorkspace(
		"/subscriptions/x/y/Microsoft.OperationalInsights/workspaces/w1")

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasTraceAxis, "Log Analytics workspace destination must flip HasTraceAxis")
	assert.True(t, snap.HasLogAxis)
	assert.Equal(t, "westus", snap.Region)
}

// Acceptance test 9 — namespace without diagnostic settings (404).
func TestServiceBusScanner_NamespaceWithoutDiagnostics_NoTraceAxis(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-bare", "eastus", "Standard"),
	}
	// Intentionally not seeding DiagSettingsByNS — fake serves 404,
	// which the scanner treats as "no diagnostic settings".

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasTraceAxis)
	assert.False(t, snaps[0].HasLogAxis)
	// And no partial failure was recorded (404 is canonical
	// "no settings" — see ScanServiceBus godoc).
}

// Event Hub destination is logging-only per the Logic Apps pattern:
// HasLogAxis flips (any setting present), HasTraceAxis stays false
// (Event Hub is neither App Insights nor Log Analytics workspace).
func TestServiceBusScanner_NamespaceWithEventHubDiagnostics_HasLogAxisButNoTraceAxis(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-eh", "eastus", "Standard"),
	}
	fake.DiagSettingsByNS["sb-eh"] = sbDiagWithEventHub(
		"/subscriptions/x/y/providers/Microsoft.EventHub/namespaces/ehns/authorizationRules/RootManageSharedAccessKey")

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasLogAxis, "Event Hub destination still counts as a structured-logging sink")
	assert.False(t, snap.HasTraceAxis,
		"Event Hub destination is logging-only — must NOT flip HasTraceAxis (Logic Apps pattern)")
}

func TestServiceBusScanner_PaginationFollowsNextLink(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.NamespacesPages = []armServiceBusNamespaceListResponse{
		{Value: []armServiceBusNamespace{makeServiceBusNamespace("sb-page1", "eastus", "Standard")}},
		{Value: []armServiceBusNamespace{makeServiceBusNamespace("sb-page2", "westus", "Premium")}},
	}
	// Both namespaces are "bare" — 404 on diag settings keeps both
	// axes false but does not impede pagination assertion.
	fake.DiagSettings404ForNS["sb-page1"] = true
	fake.DiagSettings404ForNS["sb-page2"] = true

	s, srv := newPaginatedServiceBusScanner(t, fake)
	fake.NamespacesPages[0].NextLink = fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.ServiceBus/namespaces?api-version=%s&page=2",
		srv.URL, testSubID, ServiceBusNamespacesAPIVersion,
	)

	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 2, "both pages must be walked")
	assert.Equal(t, 2, fake.NamespacesCalls, "expected two list calls (one per page)")
}

func TestServiceBusScanner_EmptyResponseReturnsEmptyResult(t *testing.T) {
	fake := newFakeAzureServiceBus()
	// Zero namespaces seeded — the fake returns an empty Value array.

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	assert.Empty(t, snaps)
	assert.Equal(t, 0, fake.DiagSettingsCalls,
		"no per-namespace diagnostic-settings calls when the namespace list is empty")
}

func TestServiceBusScanner_DiagnosticSettings404TreatedAsNoSettings(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-404", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-404"] = true

	s := newServiceBusScannerWithFake(t, fake)
	result := &scanner.Result{}
	result.AccountID = testSubID
	tok, err := s.acquireAccessToken(context.Background())
	require.NoError(t, err)
	s.ScanServiceBus(context.Background(), tok, result)

	require.Len(t, result.EventSources, 1)
	assert.False(t, result.EventSources[0].HasTraceAxis)
	assert.False(t, result.EventSources[0].HasLogAxis)
	assert.False(t, result.Partial, "404 must NOT mark the result partial")
	assert.Empty(t, result.FailedServices, "404 must NOT record a failed service")
}

func TestServiceBusScanner_ResourceNameAndARNPopulated(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-meta", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-meta"] = true

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.Equal(t, "sb-meta", snap.ResourceName, "ResourceName must be the namespace name")
	assert.Contains(t, snap.ResourceARN, "Microsoft.ServiceBus/namespaces/sb-meta",
		"ResourceARN must carry the full ARM id")
	assert.Equal(t, testSubID, snap.AccountID, "AccountID falls back to the subscription id")
}

func TestServiceBusScanner_SourceTypeIsNamespace(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-type", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-type"] = true

	s := newServiceBusScannerWithFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, "namespace", snaps[0].SourceType,
		"slice-1 chunk 3 surfaces namespaces only — SourceType must be \"namespace\"")
	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, "namespace", snaps[0].Detail["source_type"])
}

// --- Additional surgical coverage -----------------------------------

func TestServiceBusScanner_NamespacesListFailure_RecordsPartialAndNoRows(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.NamespacesListStatus = http.StatusForbidden

	s := newServiceBusScannerWithFake(t, fake)
	result := &scanner.Result{AccountID: testSubID}
	tok, err := s.acquireAccessToken(context.Background())
	require.NoError(t, err)
	s.ScanServiceBus(context.Background(), tok, result)

	assert.True(t, result.Partial, "list failure must mark the result partial")
	assert.Contains(t, result.FailedServices, ServiceIDServiceBus)
	assert.Contains(t, result.PartialReason, ServiceIDServiceBus)
	assert.Empty(t, result.EventSources)
}

func TestServiceBusScanner_PerNamespaceDiagFailure_StillSurfacesRow(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-degraded", "eastus", "Standard"),
	}
	fake.DiagSettingsStatus = http.StatusForbidden

	s := newServiceBusScannerWithFake(t, fake)
	result := &scanner.Result{AccountID: testSubID}
	tok, err := s.acquireAccessToken(context.Background())
	require.NoError(t, err)
	s.ScanServiceBus(context.Background(), tok, result)

	require.Len(t, result.EventSources, 1,
		"namespace row must still surface when the per-namespace diag call fails")
	assert.False(t, result.EventSources[0].HasTraceAxis)
	assert.False(t, result.EventSources[0].HasLogAxis)
	assert.True(t, result.Partial)
	assert.Contains(t, result.FailedServices, ServiceIDServiceBus)
}

func TestServiceBusScanner_AccountIDOverrideAndFallback(t *testing.T) {
	fake := newFakeAzureServiceBus()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-acct", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-acct"] = true

	s := newServiceBusScannerWithFake(t, fake)
	// Fallback.
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, testSubID, snaps[0].AccountID)
	// Override.
	snaps2, err2 := s.ScanEventSources(context.Background(), scanner.ScanScope{AccountID: "override-acct"})
	require.NoError(t, err2)
	require.Len(t, snaps2, 1)
	assert.Equal(t, "override-acct", snaps2[0].AccountID)
}

func TestClassifyServiceBusError_StatusMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
	}{
		{"403", &armCallError{StatusCode: http.StatusForbidden}, "permission denied"},
		{"404", &armCallError{StatusCode: http.StatusNotFound}, "subscription not found"},
		{"401", &armCallError{StatusCode: http.StatusUnauthorized}, "credentials invalid"},
		{"429", &armCallError{StatusCode: http.StatusTooManyRequests}, "rate limit"},
		{"500", &armCallError{StatusCode: http.StatusInternalServerError, Message: "boom"}, "Service Bus walk failed"},
		{"net", &armCallError{IsNetwork: true, Wrapped: fmt.Errorf("conn refused")}, "network error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.ToLower(classifyServiceBusError(tc.in))
			assert.Contains(t, got, strings.ToLower(tc.want))
			assert.Contains(t, got, ServiceIDServiceBus)
		})
	}
	assert.Empty(t, classifyServiceBusError(nil))
}

// TestServiceBusScanner_SatisfiesEventSourceSignature — defense-in-depth:
// the Azure Scanner must satisfy the optional event-source dispatcher
// signature shape. Mirrors the stub-based assertion test the handlers
// package ships against the AWS scanner.
func TestServiceBusScanner_SatisfiesEventSourceSignature(t *testing.T) {
	var fn func(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error)
	s := &Scanner{}
	fn = s.ScanEventSources
	require.NotNil(t, fn, "ScanEventSources must be assignable to the dispatcher signature")
}
