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
)

// --- Azure Functions test fake ---------------------------------------
//
// fakeAzureFunctions extends the slice-1 / slice-2 / kubernetes-tier
// transport surface with the Microsoft.Web/sites list endpoint AND
// the per-site config/appsettings/list POST sub-resource the
// serverless-tier slice 1 chunk 3 walker exercises. Parallel to
// fakeAzureSQL / fakeAzureAKS — the Functions-only test matrix runs
// against a focused mock that also routes the token / VM / SQL /
// AKS endpoints with minimal default responses so end-to-end
// "all four walked" tests run against a single fake.

type fakeAzureFunctions struct {
	mu sync.Mutex

	// Sites seeds the per-surface list response the walker
	// traverses. Empty when a test does not exercise the surface.
	Sites []armWebSite

	// SettingsBySite is the per-site app_settings map. Key is the
	// site name; value is the {key: value} app_settings map the
	// POST response packages under properties.
	SettingsBySite map[string]map[string]string

	// SitesListStatus / SitesListErrorCode / SitesListRetryAfter
	// drive the failure branches on the Microsoft.Web/sites list
	// endpoint: non-zero status returns an armErrorResponse with
	// the (defaulted) code and the optional Retry-After header.
	SitesListStatus     int
	SitesListErrorCode  string
	SitesListRetryAfter string

	// AppSettingsStatus, when non-zero, makes the per-site
	// list_application_settings call return this status for ALL
	// sites uniformly. Per-site overrides are not needed for the
	// chunk-3 test matrix.
	AppSettingsStatus int

	// AppSettingsPagesByStatus is keyed by site name; when set,
	// each entry overrides AppSettingsStatus for that specific
	// site so the multi-site partial-failure branch can be
	// exercised with one site failing and one succeeding.
	AppSettingsStatusBySite map[string]int

	// SitesPages emulates pagination — when non-empty, the list
	// endpoint serves pages from this slice (first call returns
	// pages[0], second pages[1], etc.) instead of f.Sites. The
	// final page's NextLink is the empty string to terminate
	// pagination.
	SitesPages []armWebSiteListResponse

	// Call counters and last-bearer capture for assertions.
	TokenCalls        int
	VMListCalls       int
	SQLListCalls      int
	AKSListCalls      int
	SitesListCalls    int
	AppSettingsCalls  int
	LastBearer        string
	LastAppSettingsRG string
}

func newFakeAzureFunctions() *fakeAzureFunctions {
	return &fakeAzureFunctions{
		SettingsBySite:          map[string]map[string]string{},
		AppSettingsStatusBySite: map[string]int{},
	}
}

func (f *fakeAzureFunctions) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			f.TokenCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armTokenResponse{
				AccessToken: "fake-bearer-token",
				TokenType:   "Bearer",
				ExpiresIn:   3600,
			})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.Compute/virtualMachines"):
			f.VMListCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armVMListResponse{Value: nil})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.Sql/servers"):
			f.SQLListCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armSQLServerListResponse{Value: nil})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.ContainerService/managedClusters"):
			f.AKSListCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armAKSListResponse{Value: nil})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.Web/sites"):
			f.SitesListCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.SitesListRetryAfter != "" {
				w.Header().Set("Retry-After", f.SitesListRetryAfter)
			}
			if f.SitesListStatus != 0 {
				code := f.SitesListErrorCode
				if code == "" {
					code = armErrorCodeFor(f.SitesListStatus)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.SitesListStatus)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: code, Message: armErrorMessageFor(code)},
				})
				return
			}
			// Pagination path — serve from SitesPages.
			if len(f.SitesPages) > 0 {
				idx := f.SitesListCalls - 1
				if idx >= len(f.SitesPages) {
					idx = len(f.SitesPages) - 1
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(f.SitesPages[idx])
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armWebSiteListResponse{Value: f.Sites})
			return

		case strings.Contains(path, "/providers/Microsoft.Web/sites/") && strings.HasSuffix(path, "/config/appsettings/list"):
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			f.AppSettingsCalls++
			siteName := extractSiteNameFromAppSettingsPath(path)
			f.LastAppSettingsRG = extractRGFromAppSettingsPath(path)
			// Per-site override beats global.
			status := f.AppSettingsStatusBySite[siteName]
			if status == 0 {
				status = f.AppSettingsStatus
			}
			if status != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: armErrorCodeFor(status), Message: "app settings failure"},
				})
				return
			}
			settings := f.SettingsBySite[siteName]
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armAppSettingsResponse{Properties: settings})
			return
		}

		// Object-store + load-balancer tiers run after the serverless
		// walk; route them empty so Functions tests don't get spurious
		// azurestorage / azurelb partial failures.
		if routeEmptyInfraTier(w, path) {
			return
		}

		// Unhandled.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(armErrorResponse{
			Error: armErrorBody{Code: "NotFound", Message: fmt.Sprintf("unhandled mock path: %s", path)},
		})
	})
}

func newFunctionsScannerWithFake(t *testing.T, fake *fakeAzureFunctions) *Scanner {
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

// extractSiteNameFromAppSettingsPath pulls the {site} segment out of
// the per-site list_application_settings URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Web/sites/<site>/config/appsettings/list
func extractSiteNameFromAppSettingsPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "sites" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// extractRGFromAppSettingsPath pulls the {rg} segment.
func extractRGFromAppSettingsPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "resourceGroups" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// --- Helpers ----------------------------------------------------------

// makeFunctionApp constructs an armWebSite with the slice-1 chunk-3
// detection-rule-relevant fields plumbed through. Kind is configurable
// so the filter test can mix in non-Function-App entries (kind="app"
// / kind="app,linux").
func makeFunctionApp(name, location, kind, linuxFx, windowsFx string, tags map[string]string) armWebSite {
	return armWebSite{
		ID:       fmt.Sprintf("/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-%s/providers/Microsoft.Web/sites/%s", name, name),
		Name:     name,
		Location: location,
		Kind:     kind,
		Tags:     tags,
		Properties: &armWebSiteProps{
			SiteConfig: &armWebSiteConfig{
				LinuxFxVersion:   linuxFx,
				WindowsFxVersion: windowsFx,
			},
		},
	}
}

// --- Acceptance tests 7-8 ---------------------------------------------
//
// docs/proposals/serverless-tier-slice1.md §11 acceptance tests:
//   7. Azure Functions scanner — function with App Insights.
//      Assert: HasTraceAxis=true.
//   8. Azure Functions scanner — function with OTel distro env.
//      Assert: HasOTelDistro=true.

func TestFunctionAppScanner_FunctionWithAppInsights_HasTraceAxis(t *testing.T) {
	// Acceptance test 7.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-prod", "eastus", "functionapp,linux", "Python|3.11", "", nil),
	}
	fake.SettingsBySite["fn-prod"] = map[string]string{
		AppInsightsConnectionStringAppSetting: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://eastus-1.in.applicationinsights.azure.com/",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]
	assert.True(t, snap.HasTraceAxis, "APPLICATIONINSIGHTS_CONNECTION_STRING present must flip HasTraceAxis")
	assert.False(t, snap.HasOTelDistro, "no OTel distro app_setting → HasOTelDistro=false")
	assert.Equal(t, azureFunctionsServerlessSurface, snap.Surface)
	assert.Equal(t, azureProviderID, snap.Provider)
	assert.Equal(t, "fn-prod", snap.ResourceName)
	assert.Equal(t, "eastus", snap.Region)
}

func TestFunctionAppScanner_FunctionWithoutAppInsights_NoTraceAxis(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-no-ai", "eastus", "functionapp,linux", "Python|3.11", "", nil),
	}
	// SettingsBySite["fn-no-ai"] absent — list_application_settings
	// returns an empty properties map.

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]
	assert.False(t, snap.HasTraceAxis)
	assert.False(t, snap.HasOTelDistro)
}

func TestFunctionAppScanner_FunctionWithOTelDotNetDistro_HasOTelDistro(t *testing.T) {
	// Acceptance test 8 — .NET branch.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-dn", "eastus", "functionapp,linux", "DotNet|6.0", "", nil),
	}
	fake.SettingsBySite["fn-dn"] = map[string]string{
		OTelDotNetAutoHomeAppSetting: "/home/site/wwwroot/otel-dotnet-auto",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]
	assert.True(t, snap.HasOTelDistro, "OTEL_DOTNET_AUTO_HOME present must flip HasOTelDistro")
	assert.False(t, snap.HasTraceAxis)
}

func TestFunctionAppScanner_FunctionWithOTelPythonDistro_HasOTelDistro(t *testing.T) {
	// Acceptance test 8 — Python branch.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-py", "eastus", "functionapp,linux", "Python|3.11", "", nil),
	}
	fake.SettingsBySite["fn-py"] = map[string]string{
		OTelPythonDistroAppSetting: "opentelemetry-distro",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]
	assert.True(t, snap.HasOTelDistro, "OTEL_PYTHON_DISTRO present must flip HasOTelDistro")
	assert.False(t, snap.HasTraceAxis)
}

func TestFunctionAppScanner_FunctionWithNeitherDistro_NoOTelDistro(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-bare", "eastus", "functionapp,linux", "Node|18", "", nil),
	}
	// Other app_settings present but none of the load-bearing keys.
	fake.SettingsBySite["fn-bare"] = map[string]string{
		"FUNCTIONS_EXTENSION_VERSION":  "~4",
		"WEBSITE_NODE_DEFAULT_VERSION": "~18",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]
	assert.False(t, snap.HasTraceAxis)
	assert.False(t, snap.HasOTelDistro)
	assert.False(t, snap.IsInstrumented(), "neither axis on → IsInstrumented=false")
}

func TestFunctionAppScanner_FunctionWithBothEnvVars_OneIsEnough(t *testing.T) {
	// Disjunction sanity — both keys present still flips
	// HasOTelDistro=true (not "true twice"). Mirrors the AWS Lambda
	// chunk-1 dual-rule test.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-both", "eastus", "functionapp,linux", "DotNet|6.0", "", nil),
	}
	fake.SettingsBySite["fn-both"] = map[string]string{
		OTelDotNetAutoHomeAppSetting: "/home/site/wwwroot/otel-dotnet-auto",
		OTelPythonDistroAppSetting:   "opentelemetry-distro",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	assert.True(t, res.Serverless[0].HasOTelDistro)
}

func TestFunctionAppScanner_BothAxesOn_IsInstrumentedTrue(t *testing.T) {
	// The OR-rule on ServerlessInstanceSnapshot.IsInstrumented:
	// both axes on still counts as instrumented (not "doubly so").
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-all", "eastus", "functionapp,linux", "Python|3.11", "", nil),
	}
	fake.SettingsBySite["fn-all"] = map[string]string{
		AppInsightsConnectionStringAppSetting: "InstrumentationKey=key",
		OTelPythonDistroAppSetting:            "opentelemetry-distro",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]
	assert.True(t, snap.HasTraceAxis)
	assert.True(t, snap.HasOTelDistro)
	assert.True(t, snap.IsInstrumented())
}

func TestFunctionAppScanner_AppInsightsKeyPresentButEmpty_NoTraceAxis(t *testing.T) {
	// Empty-string value is treated as "not set" — the slice 1
	// contract reports on workable connection-string presence,
	// not just key existence.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-blank", "eastus", "functionapp,linux", "Python|3.11", "", nil),
	}
	fake.SettingsBySite["fn-blank"] = map[string]string{
		AppInsightsConnectionStringAppSetting: "",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 1)
	assert.False(t, res.Serverless[0].HasTraceAxis, "empty value treated as not-set")
}

// --- Kind filter ------------------------------------------------------

func TestFunctionAppScanner_KindFilter_OnlyIncludesFunctionApps(t *testing.T) {
	// Mocks a Web App, a Function App, and a Linux Function App in
	// the same subscription. Only the two Function Apps appear in
	// Result.Serverless.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("web-app", "eastus", "app", "", "", nil),
		makeFunctionApp("web-app-linux", "eastus", "app,linux", "PYTHON|3.11", "", nil),
		makeFunctionApp("fn-app", "eastus", "functionapp", "", "dotnet:6.0", nil),
		makeFunctionApp("fn-app-linux", "eastus", "functionapp,linux", "Python|3.11", "", nil),
		makeFunctionApp("fn-workflow", "westus2", "functionapp,workflowapp", "", "", nil),
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 3, "only the three functionapp* sites must surface")
	byName := map[string]bool{}
	for _, sv := range res.Serverless {
		byName[sv.ResourceName] = true
	}
	assert.True(t, byName["fn-app"])
	assert.True(t, byName["fn-app-linux"])
	assert.True(t, byName["fn-workflow"])
	assert.False(t, byName["web-app"])
	assert.False(t, byName["web-app-linux"])
}

// --- Runtime parsing --------------------------------------------------

func TestFunctionAppScanner_RuntimeFieldParsed_Linux(t *testing.T) {
	// Per the chunk-3 brief: "Python|3.11" → "python3.11";
	// "Node|18" → "node18"; "DOTNET-ISOLATED|6.0" → "dotnet6.0".
	cases := []struct {
		name    string
		linuxFx string
		want    string
	}{
		{"python", "Python|3.11", "python3.11"},
		{"node", "Node|18", "node18"},
		{"dotnet", "DotNet|6.0", "dotnet6.0"},
		{"dotnet-isolated", "DOTNET-ISOLATED|6.0", "dotnet6.0"},
		{"java", "Java|17", "java17"},
		{"powershell", "PowerShell|7.2", "powershell7.2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAzureFunctions()
			fake.Sites = []armWebSite{
				makeFunctionApp("fn-"+tc.name, "eastus", "functionapp,linux", tc.linuxFx, "", nil),
			}

			s := newFunctionsScannerWithFake(t, fake)
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Serverless, 1)
			assert.Equal(t, tc.want, res.Serverless[0].Runtime)
		})
	}
}

func TestFunctionAppScanner_RuntimeFieldParsed_Windows(t *testing.T) {
	// Windows: WindowsFxVersion uses "family:version"; absent or empty
	// LinuxFxVersion falls through to the Windows field, then to
	// FUNCTIONS_WORKER_RUNTIME.
	t.Run("WindowsFxVersion-dotnet-colon-form", func(t *testing.T) {
		fake := newFakeAzureFunctions()
		fake.Sites = []armWebSite{
			makeFunctionApp("fn-win", "eastus", "functionapp", "", "dotnet:6.0", nil),
		}

		s := newFunctionsScannerWithFake(t, fake)
		res, err := s.Scan(context.Background())
		require.NoError(t, err)
		require.Len(t, res.Serverless, 1)
		assert.Equal(t, "dotnet6.0", res.Serverless[0].Runtime)
	})

	t.Run("FUNCTIONS_WORKER_RUNTIME-fallback", func(t *testing.T) {
		fake := newFakeAzureFunctions()
		fake.Sites = []armWebSite{
			makeFunctionApp("fn-win-fallback", "eastus", "functionapp", "", "", nil),
		}
		fake.SettingsBySite["fn-win-fallback"] = map[string]string{
			FunctionsWorkerRuntimeAppSetting: "dotnet-isolated",
		}

		s := newFunctionsScannerWithFake(t, fake)
		res, err := s.Scan(context.Background())
		require.NoError(t, err)
		require.Len(t, res.Serverless, 1)
		// Bare worker identifier (no version suffix); lowercased.
		assert.Equal(t, "dotnet-isolated", res.Serverless[0].Runtime)
	})

	t.Run("no-runtime-signal", func(t *testing.T) {
		fake := newFakeAzureFunctions()
		fake.Sites = []armWebSite{
			makeFunctionApp("fn-empty", "eastus", "functionapp", "", "", nil),
		}
		// SettingsBySite empty — list_application_settings returns
		// nil properties.

		s := newFunctionsScannerWithFake(t, fake)
		res, err := s.Scan(context.Background())
		require.NoError(t, err)
		require.Len(t, res.Serverless, 1)
		assert.Equal(t, "", res.Serverless[0].Runtime)
	})
}

// --- Pagination -------------------------------------------------------

func TestFunctionAppScanner_PaginationFollowsNextLink(t *testing.T) {
	fake := newFakeAzureFunctions()
	page1 := armWebSiteListResponse{
		Value: []armWebSite{
			makeFunctionApp("fn-1", "eastus", "functionapp,linux", "Python|3.11", "", nil),
			makeFunctionApp("not-a-fn", "eastus", "app", "", "", nil), // Filtered out client-side.
		},
		NextLink: "PLACEHOLDER", // replaced after server starts so we can use srv.URL.
	}
	page2 := armWebSiteListResponse{
		Value: []armWebSite{
			makeFunctionApp("fn-2", "westus2", "functionapp", "", "dotnet:6.0", nil),
		},
		NextLink: "",
	}
	fake.SitesPages = []armWebSiteListResponse{page1, page2}

	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	// Patch the page1.NextLink to point back at the fake server.
	page1.NextLink = srv.URL + "/subscriptions/22222222-2222-2222-2222-222222222222/providers/Microsoft.Web/sites?api-version=" + armWebAppListAPIVersion + "&page=2"
	fake.SitesPages[0] = page1

	s := &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}

	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 2, "pagination must follow nextLink and accumulate across pages")
	assert.Equal(t, 2, fake.SitesListCalls, "two pages = two list calls")
	names := map[string]bool{}
	for _, sv := range res.Serverless {
		names[sv.ResourceName] = true
	}
	assert.True(t, names["fn-1"])
	assert.True(t, names["fn-2"])
}

// --- Snapshot shape ---------------------------------------------------

func TestFunctionAppScanner_SnapshotShape_PopulatedFields(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-shape", "eastus", "functionapp,linux", "Python|3.11", "", map[string]string{"team": "platform"}),
	}
	fake.SettingsBySite["fn-shape"] = map[string]string{
		AppInsightsConnectionStringAppSetting: "InstrumentationKey=abc",
		OTelPythonDistroAppSetting:            "opentelemetry-distro",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Serverless, 1)
	snap := res.Serverless[0]

	assert.Equal(t, azureProviderID, snap.Provider)
	assert.Equal(t, azureFunctionsServerlessSurface, snap.Surface)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", snap.AccountID)
	assert.Equal(t, "eastus", snap.Region)
	assert.Equal(t, "fn-shape", snap.ResourceName)
	assert.Contains(t, snap.ResourceARN, "Microsoft.Web/sites/fn-shape")
	assert.Equal(t, "python3.11", snap.Runtime)
	assert.True(t, snap.HasTraceAxis)
	assert.True(t, snap.HasOTelDistro)
	require.NotNil(t, snap.Detail)
	assert.Equal(t, "functionapp,linux", snap.Detail["kind"])
	assert.Equal(t, "Python|3.11", snap.Detail["linux_fx_version"])
	assert.Equal(t, true, snap.Detail["has_app_insights"])
	assert.Equal(t, true, snap.Detail["has_otel_python"])
	assert.Equal(t, false, snap.Detail["has_otel_dotnet"])
}

// --- Partial-failure semantics ---------------------------------------

func TestFunctionAppScanner_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.SitesListStatus = http.StatusForbidden
	fake.SitesListErrorCode = "AuthorizationFailed"

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied at sites list is a partial-failure surface")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "Reader role")
	assert.Contains(t, res.FailedServices, ServiceIDAzureFunctions)
	assert.Empty(t, res.Serverless)
}

func TestFunctionAppScanner_RateLimited_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.SitesListStatus = http.StatusTooManyRequests

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDAzureFunctions)
	assert.Empty(t, res.Serverless)
}

func TestFunctionAppScanner_EmptySitesList_NoPartialFailure(t *testing.T) {
	fake := newFakeAzureFunctions()
	// fake.Sites stays nil — endpoint returns Value: [].

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.False(t, res.Partial, "empty Function Apps inventory is NOT a partial failure")
	assert.Empty(t, res.PartialReason)
	assert.NotContains(t, res.FailedServices, ServiceIDAzureFunctions)
	assert.Empty(t, res.Serverless)
	assert.Equal(t, 1, fake.SitesListCalls, "the endpoint was still hit")
}

func TestFunctionAppScanner_AppSettingsFailure_AppendsRowAndRecordsPartial(t *testing.T) {
	// list_application_settings 403 → partial failure recorded, BUT
	// the inventory row is still appended (HasTraceAxis=false,
	// HasOTelDistro=false) so the operator sees the function exists
	// and can fix the policy. Squadron's invariant: err toward
	// visibility on partial failures.
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-1", "eastus", "functionapp,linux", "Python|3.11", "", nil),
		makeFunctionApp("fn-2", "eastus", "functionapp,linux", "Node|18", "", nil),
	}
	fake.AppSettingsStatusBySite["fn-1"] = http.StatusForbidden
	fake.SettingsBySite["fn-2"] = map[string]string{
		AppInsightsConnectionStringAppSetting: "InstrumentationKey=ok",
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 2, "both sites surface as rows")
	assert.True(t, res.Partial)
	assert.Contains(t, res.FailedServices, ServiceIDAzureFunctions)
	byName := map[string]int{}
	for i, sv := range res.Serverless {
		byName[sv.ResourceName] = i
	}
	require.Contains(t, byName, "fn-1")
	require.Contains(t, byName, "fn-2")
	fn1 := res.Serverless[byName["fn-1"]]
	assert.False(t, fn1.HasTraceAxis, "settings call failed → axes stay false")
	assert.False(t, fn1.HasOTelDistro)
	require.NotNil(t, fn1.Detail)
	assert.Equal(t, true, fn1.Detail["settings_unread"], "Detail surfaces the unread-settings sentinel")
	fn2 := res.Serverless[byName["fn-2"]]
	assert.True(t, fn2.HasTraceAxis, "second site's successful settings call still flips its axis")
}

// --- Direct projection-helper tests ----------------------------------

func TestProjectFunctionApp_DetectionMatrix(t *testing.T) {
	// Pin every combination of the two axes' presence/absence so a
	// future refactor that reorders the rule or swaps the
	// short-circuit can't regress the contract silently.
	cases := []struct {
		name           string
		settings       map[string]string
		wantTraceAxis  bool
		wantOTelDistro bool
	}{
		{
			name:           "empty",
			settings:       map[string]string{},
			wantTraceAxis:  false,
			wantOTelDistro: false,
		},
		{
			name:           "app insights only",
			settings:       map[string]string{AppInsightsConnectionStringAppSetting: "key=v"},
			wantTraceAxis:  true,
			wantOTelDistro: false,
		},
		{
			name:           "otel dotnet only",
			settings:       map[string]string{OTelDotNetAutoHomeAppSetting: "/path"},
			wantTraceAxis:  false,
			wantOTelDistro: true,
		},
		{
			name:           "otel python only",
			settings:       map[string]string{OTelPythonDistroAppSetting: "opentelemetry-distro"},
			wantTraceAxis:  false,
			wantOTelDistro: true,
		},
		{
			name: "all three",
			settings: map[string]string{
				AppInsightsConnectionStringAppSetting: "key=v",
				OTelDotNetAutoHomeAppSetting:          "/path",
				OTelPythonDistroAppSetting:            "opentelemetry-distro",
			},
			wantTraceAxis:  true,
			wantOTelDistro: true,
		},
		{
			name: "all three keys with empty values",
			settings: map[string]string{
				AppInsightsConnectionStringAppSetting: "",
				OTelDotNetAutoHomeAppSetting:          "",
				OTelPythonDistroAppSetting:            "",
			},
			wantTraceAxis:  false,
			wantOTelDistro: false,
		},
	}

	site := makeFunctionApp("test", "eastus", "functionapp,linux", "Python|3.11", "", nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := projectFunctionApp(site, tc.settings, "account-id")
			assert.Equal(t, tc.wantTraceAxis, snap.HasTraceAxis)
			assert.Equal(t, tc.wantOTelDistro, snap.HasOTelDistro)
		})
	}
}

func TestNormalizeFunctionAppRuntime_Cases(t *testing.T) {
	cases := []struct {
		name          string
		linuxFx       string
		windowsFx     string
		workerRuntime string
		want          string
	}{
		{"linux python", "Python|3.11", "", "", "python3.11"},
		{"linux node", "Node|18", "", "", "node18"},
		{"linux dotnet-isolated", "DOTNET-ISOLATED|6.0", "", "", "dotnet6.0"},
		{"windows colon form", "", "dotnet:6.0", "", "dotnet6.0"},
		{"worker runtime fallback", "", "", "python", "python"},
		{"worker runtime uppercase normalized", "", "", "DOTNET-ISOLATED", "dotnet-isolated"},
		{"linux wins over windows", "Python|3.11", "dotnet:6.0", "python", "python3.11"},
		{"windows wins over worker", "", "dotnet:6.0", "python", "dotnet6.0"},
		{"empty all", "", "", "", ""},
		{"unknown family pass-through", "FrobnitzLang|9.9", "", "", "frobnitzlang9.9"},
		{"linux family-only no version", "Python", "", "", "python"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeFunctionAppRuntime(tc.linuxFx, tc.windowsFx, tc.workerRuntime)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIsFunctionApp_KindMatrix(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{"functionapp", true},
		{"functionapp,linux", true},
		{"functionapp,workflowapp", true},
		{"functionapp,linux,container", true},
		{"app", false},
		{"app,linux", false},
		{"app,linux,container", false},
		{"", false},
		{"FunctionApp", false}, // case-sensitive — Azure publishes lowercase.
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			site := armWebSite{Kind: tc.kind}
			assert.Equal(t, tc.want, isFunctionApp(site))
		})
	}
}

// --- ProviderID assertion --------------------------------------------

func TestFunctionAppScanner_ProviderFieldSet(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn-a", "eastus", "functionapp,linux", "Python|3.11", "", nil),
		makeFunctionApp("fn-b", "westus2", "functionapp", "", "dotnet:6.0", nil),
	}

	s := newFunctionsScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Serverless, 2)
	for _, sv := range res.Serverless {
		assert.Equal(t, "azure", sv.Provider)
		assert.Equal(t, "azfunc", sv.Surface)
	}
}

// --- Token-bearer propagation ----------------------------------------

func TestFunctionAppScanner_BearerTokenPropagated(t *testing.T) {
	fake := newFakeAzureFunctions()
	fake.Sites = []armWebSite{
		makeFunctionApp("fn", "eastus", "functionapp,linux", "Python|3.11", "", nil),
	}

	s := newFunctionsScannerWithFake(t, fake)
	_, err := s.Scan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer fake-bearer-token", fake.LastBearer)
}
