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

// --- SQL test fake ---------------------------------------------------
//
// fakeAzureSQL extends the slice-1 fakeAzure transport surface with
// the three Microsoft.Sql / Diagnostic Settings endpoints the
// chunk-3 walker exercises. It is structured as a separate fake
// rather than bolted onto fakeAzure so the SQL test cases stay
// surgical — no risk of accidentally affecting the slice-1 VM walk
// tests, and the assertions read against a focused mock.

type fakeAzureSQL struct {
	mu sync.Mutex

	// Servers is the static SQL Server list served by the
	// subscription-scope Microsoft.Sql/servers list endpoint.
	Servers []armSQLServer

	// VMs feed the slice-1 endpoint so end-to-end tests can seed
	// both surfaces in a single fake. Empty when the test only
	// exercises the SQL walk.
	VMs []armVirtualMachine

	// DatabasesByServer keys per-server name → database list
	// returned by the per-server Microsoft.Sql/servers/{server}/
	// databases endpoint.
	DatabasesByServer map[string][]armSQLDatabase

	// DiagSettingsByDBPath keys the trailing path component (the
	// database name) to the diagnostic settings response served by
	// microsoft.insights/diagnosticSettings. A missing key returns
	// 404 — the canonical "no settings" Azure shape, which the
	// walker treats as SQLInsightsDiagEnabled=false.
	DiagSettingsByDBPath map[string]armDiagnosticSettingsResponse

	// SQLServersStatus, when non-zero, makes the SQL server list
	// endpoint return this status with an armErrorResponse body.
	SQLServersStatus     int
	SQLServersErrorCode  string
	SQLServersRetryAfter string

	// DatabasesStatus, when non-zero, makes the per-server
	// databases list endpoint return this status. Applied uniformly
	// across every server's database list call; per-server status
	// overrides are not needed for the chunk-3 test matrix.
	DatabasesStatus int

	// DiagSettingsStatus, when non-zero AND not 404, makes the
	// diagnostic settings endpoint return this status (for the
	// partial-failure tests that probe diag-settings failures
	// distinct from the canonical 404).
	DiagSettingsStatus int

	// Call counters for assertions.
	TokenCalls          int
	VMListCalls         int
	SQLServersCalls     int
	SQLDatabasesCalls   int
	DiagSettingsCalls   int
	LastSQLServersPath  string
	LastDatabasesPath   string
	LastDiagSettingsURL string
}

func newFakeAzureSQL() *fakeAzureSQL {
	return &fakeAzureSQL{
		DatabasesByServer:    map[string][]armSQLDatabase{},
		DiagSettingsByDBPath: map[string]armDiagnosticSettingsResponse{},
	}
}

func (f *fakeAzureSQL) handler() http.Handler {
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
			_ = json.NewEncoder(w).Encode(armVMListResponse{Value: f.VMs})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.Sql/servers"):
			f.SQLServersCalls++
			f.LastSQLServersPath = path
			if f.SQLServersRetryAfter != "" {
				w.Header().Set("Retry-After", f.SQLServersRetryAfter)
			}
			if f.SQLServersStatus != 0 {
				code := f.SQLServersErrorCode
				if code == "" {
					code = armErrorCodeFor(f.SQLServersStatus)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.SQLServersStatus)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: code, Message: armErrorMessageFor(code)},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armSQLServerListResponse{Value: f.Servers})
			return

		case strings.Contains(path, "/providers/Microsoft.Sql/servers/") && strings.HasSuffix(path, "/databases"):
			f.SQLDatabasesCalls++
			f.LastDatabasesPath = path
			if f.DatabasesStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.DatabasesStatus)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: armErrorCodeFor(f.DatabasesStatus), Message: "failure"},
				})
				return
			}
			serverName := extractServerNameFromDBListPath(path)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armSQLDatabaseListResponse{
				Value: f.DatabasesByServer[serverName],
			})
			return

		case strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			f.DiagSettingsCalls++
			f.LastDiagSettingsURL = path
			if f.DiagSettingsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.DiagSettingsStatus)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: armErrorCodeFor(f.DiagSettingsStatus), Message: "diag failure"},
				})
				return
			}
			dbName := extractDBNameFromDiagPath(path)
			settings, ok := f.DiagSettingsByDBPath[dbName]
			if !ok {
				// Canonical "no settings configured" 404.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: "NoSettings", Message: "no diagnostic settings"},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(settings)
			return

		case strings.HasSuffix(path, "/providers/Microsoft.ContainerService/managedClusters"):
			// Kubernetes-tier-slice-2 (chunk 3): the SQL-walk fake
			// also routes the AKS managedClusters list endpoint so
			// slice-2 SQL tests don't get spurious "aks" partial
			// failures from the chunk-3 walker. The default response
			// is an empty cluster list — operator has no AKS
			// inventory.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armAKSListResponse{Value: nil})
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

// extractServerNameFromDBListPath pulls the {server} segment out of
// the per-server databases list URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Sql/servers/<server>/databases
func extractServerNameFromDBListPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "servers" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// extractDBNameFromDiagPath pulls the {db} segment out of the
// database-scope diagnostic settings URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Sql/servers/<server>/databases/<db>/providers/microsoft.insights/diagnosticSettings
func extractDBNameFromDiagPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "databases" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func newSQLScannerWithFake(t *testing.T, fake *fakeAzureSQL) *Scanner {
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

// --- Helpers ----------------------------------------------------------

func makeSQLServer(name, location string) armSQLServer {
	return armSQLServer{
		ID: fmt.Sprintf(
			"/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-%s/providers/Microsoft.Sql/servers/%s",
			name, name),
		Name:     name,
		Location: location,
	}
}

func makeSQLDatabase(serverName, dbName, location, sku, currentObjective string, tags map[string]string) armSQLDatabase {
	return armSQLDatabase{
		ID: fmt.Sprintf(
			"/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-%s/providers/Microsoft.Sql/servers/%s/databases/%s",
			serverName, serverName, dbName),
		Name:     dbName,
		Location: location,
		Tags:     tags,
		Sku:      armSQLSku{Name: sku},
		Properties: armSQLDatabaseProp{
			CurrentServiceObjectiveName: currentObjective,
		},
	}
}

func diagSettingsWithCategory(category string, enabled bool) armDiagnosticSettingsResponse {
	return armDiagnosticSettingsResponse{
		Value: []armDiagnosticSetting{{
			Name: "ds-1",
			Properties: armDiagnosticSettingProp{
				Logs: []armDiagnosticLog{
					{Category: category, Enabled: enabled},
				},
			},
		}},
	}
}

// --- Tests ------------------------------------------------------------

func TestScan_AzureSQL_ReturnsDatabaseInstanceSnapshot(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("sqlsrv-1", "eastus")}
	fake.DatabasesByServer["sqlsrv-1"] = []armSQLDatabase{
		makeSQLDatabase("sqlsrv-1", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", map[string]string{"env": "prod"}),
		makeSQLDatabase("sqlsrv-1", "reports", "eastus", "S2", "Standard_S2", nil),
	}

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 2)
	ids := map[string]bool{}
	for _, d := range res.Databases {
		ids[d.ResourceID] = true
	}
	assert.True(t, ids["sqlsrv-1/app-db"], "expected server-qualified id sqlsrv-1/app-db")
	assert.True(t, ids["sqlsrv-1/reports"], "expected server-qualified id sqlsrv-1/reports")

	for _, d := range res.Databases {
		assert.Equal(t, "sqlserver", d.Engine)
		assert.Equal(t, "azure", d.Provider)
		assert.Equal(t, "eastus", d.Region)
	}
}

func TestScan_AzureSQL_SQLInsightsEnabled_DetectsHasInsights(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	fake.DiagSettingsByDBPath["app-db"] = diagSettingsWithCategory(sqlInsightsCategory, true)

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.True(t, res.Databases[0].SQLInsightsDiagEnabled)
	assert.False(t, res.Partial)
}

func TestScan_AzureSQL_NoSQLInsightsCategory_DetectsAsFalse(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	// Diagnostic setting present but routing only AutomaticTuning,
	// no SQLInsights.
	fake.DiagSettingsByDBPath["app-db"] = diagSettingsWithCategory("AutomaticTuning", true)

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.False(t, res.Databases[0].SQLInsightsDiagEnabled)
	assert.False(t, res.Partial)
}

func TestScan_AzureSQL_SQLInsightsDisabled_DetectsAsFalse(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	// SQLInsights category present but Enabled=false.
	fake.DiagSettingsByDBPath["app-db"] = diagSettingsWithCategory(sqlInsightsCategory, false)

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.False(t, res.Databases[0].SQLInsightsDiagEnabled)
	assert.False(t, res.Partial)
}

func TestScan_AzureSQL_NoDiagSettingsResponse404_DetectsAsFalse(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	// Intentionally not seeding DiagSettingsByDBPath — the fake
	// returns 404 for any database it doesn't know about.

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.False(t, res.Databases[0].SQLInsightsDiagEnabled)
	assert.False(t, res.Partial, "404 on diag settings is NOT a partial failure")
	assert.Empty(t, res.FailedServices)
}

func TestScan_AzureSQL_MasterDatabaseSkipped(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "master", "eastus", "System", "System0", nil),
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1, "master should be skipped")
	assert.Equal(t, "srv/app-db", res.Databases[0].ResourceID)
}

func TestScan_AzureSQL_MultipleDestinations_OneSQLInsightsSuffices(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	// Two diagnostic settings: first routes only Errors, second
	// routes SQLInsights enabled. The rule is "at least one"
	// flips the flag.
	fake.DiagSettingsByDBPath["app-db"] = armDiagnosticSettingsResponse{
		Value: []armDiagnosticSetting{
			{
				Name: "ds-storage",
				Properties: armDiagnosticSettingProp{
					Logs: []armDiagnosticLog{{Category: "Errors", Enabled: true}},
				},
			},
			{
				Name: "ds-loganalytics",
				Properties: armDiagnosticSettingProp{
					Logs: []armDiagnosticLog{
						{Category: "AutomaticTuning", Enabled: true},
						{Category: sqlInsightsCategory, Enabled: true},
					},
				},
			},
		},
	}

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.True(t, res.Databases[0].SQLInsightsDiagEnabled)
}

func TestScan_AzureSQL_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.SQLServersStatus = http.StatusForbidden
	fake.SQLServersErrorCode = "AuthorizationFailed"

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.FailedServices, ServiceIDAzureSQL)
	assert.Empty(t, res.Databases)
}

func TestScan_AzureSQL_RateLimited_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.SQLServersStatus = http.StatusTooManyRequests

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDAzureSQL)
}

func TestScan_VMsAndAzureSQL_BothWalked(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.VMs = []armVirtualMachine{
		makeVM("web-1", "eastus", "Standard_D4s_v3", "Linux", map[string]string{"otel": "v1"}),
	}
	fake.Servers = []armSQLServer{makeSQLServer("sqlsrv", "eastus")}
	fake.DatabasesByServer["sqlsrv"] = []armSQLDatabase{
		makeSQLDatabase("sqlsrv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	fake.DiagSettingsByDBPath["app-db"] = diagSettingsWithCategory(sqlInsightsCategory, true)

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 1)
	require.Len(t, res.Databases, 1)
	assert.True(t, res.Compute[0].HasOTel)
	assert.True(t, res.Databases[0].SQLInsightsDiagEnabled)
	assert.False(t, res.Partial)
	assert.Equal(t, 1, fake.VMListCalls)
	assert.Equal(t, 1, fake.SQLServersCalls)
}

func TestScan_AzureSQL_ProviderFieldSet(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv-a", "eastus"), makeSQLServer("srv-b", "westus2")}
	fake.DatabasesByServer["srv-a"] = []armSQLDatabase{
		makeSQLDatabase("srv-a", "db1", "eastus", "S0", "Standard_S0", nil),
	}
	fake.DatabasesByServer["srv-b"] = []armSQLDatabase{
		makeSQLDatabase("srv-b", "db2", "westus2", "S2", "Standard_S2", nil),
	}

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 2)
	for _, d := range res.Databases {
		assert.Equal(t, "azure", d.Provider, "every snapshot Provider field must be azure")
	}
}

func TestScan_AzureSQL_EmptyServerList_NoPartialFailure(t *testing.T) {
	fake := newFakeAzureSQL()
	// fake.Servers stays nil — the endpoint returns Value: [].

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	assert.False(t, res.Partial)
	assert.Empty(t, res.FailedServices)
	assert.Empty(t, res.Databases)
	assert.Equal(t, 1, fake.SQLServersCalls)
	assert.Equal(t, 0, fake.SQLDatabasesCalls, "no servers → no per-server database calls")
}

// --- parseRGFromARMID direct tests -----------------------------------

func TestParseRGFromARMID_Cases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "canonical lowercase resourceGroups segment",
			in:   "/subscriptions/abc/resourceGroups/my-rg/providers/Microsoft.Sql/servers/srv",
			want: "my-rg",
		},
		{
			name: "all-lowercase resourcegroups variant",
			in:   "/subscriptions/abc/resourcegroups/my-rg/providers/Microsoft.Sql/servers/srv",
			want: "my-rg",
		},
		{
			name: "all-uppercase RESOURCEGROUPS variant",
			in:   "/SUBSCRIPTIONS/abc/RESOURCEGROUPS/my-rg/providers/Microsoft.Sql/servers/srv",
			want: "my-rg",
		},
		{
			name: "rg with mixed case name preserved",
			in:   "/subscriptions/abc/resourceGroups/My-RG-PROD/providers/Microsoft.Sql/servers/srv",
			want: "My-RG-PROD",
		},
		{
			name: "deeper path with sub-resources",
			in:   "/subscriptions/abc/resourceGroups/rg/providers/Microsoft.Sql/servers/srv/databases/db",
			want: "rg",
		},
		{
			name: "no resourceGroups segment returns empty",
			in:   "/subscriptions/abc/providers/Microsoft.Sql",
			want: "",
		},
		{
			name: "empty id returns empty",
			in:   "",
			want: "",
		},
		{
			name: "resourceGroups segment at trailing position returns empty",
			in:   "/subscriptions/abc/resourceGroups",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseRGFromARMID(tc.in))
		})
	}
}

// --- Extra coverage for projection edge cases ------------------------

// TestScan_AzureSQL_EngineVersionFallsBackToSku exercises the
// projection rule that EngineVersion uses sku.name when
// currentServiceObjectiveName is empty. Pins the documented fallback
// so a future scanner change doesn't silently emit empty version
// strings on freshly-created databases.
func TestScan_AzureSQL_EngineVersionFallsBackToSku(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		// currentObjective intentionally empty.
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "", nil),
	}

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.Equal(t, "GP_S_Gen5_2", res.Databases[0].EngineVersion)
	assert.Equal(t, "GP_S_Gen5_2", res.Databases[0].InstanceClass)
}

// TestScan_AzureSQL_DiagSettings5xx_RecordsPartialFailureAndAppendsRow
// pins the invariant that a non-404 failure on the diagnostic
// settings probe records a partial failure AND still appends the
// database row with SQLInsightsDiagEnabled=false. Squadron's
// inventory-visibility rule errs toward surfacing rows when the
// projection can be partly populated, rather than dropping them on
// mid-scan failures.
func TestScan_AzureSQL_DiagSettings5xx_RecordsPartialFailureAndAppendsRow(t *testing.T) {
	fake := newFakeAzureSQL()
	fake.Servers = []armSQLServer{makeSQLServer("srv", "eastus")}
	fake.DatabasesByServer["srv"] = []armSQLDatabase{
		makeSQLDatabase("srv", "app-db", "eastus", "GP_S_Gen5_2", "GP_S_Gen5_2", nil),
	}
	fake.DiagSettingsStatus = http.StatusInternalServerError

	s := newSQLScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	assert.True(t, res.Partial)
	assert.Contains(t, res.FailedServices, ServiceIDAzureSQL)
	require.Len(t, res.Databases, 1)
	assert.False(t, res.Databases[0].SQLInsightsDiagEnabled)
}
