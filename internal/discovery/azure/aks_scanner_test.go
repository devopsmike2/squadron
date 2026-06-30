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

// --- AKS test fake ---------------------------------------------------
//
// fakeAzureAKS extends the slice-1 / slice-2 transport surface with
// the Microsoft.ContainerService/managedClusters endpoint the
// kubernetes-tier-slice-2 (chunk 3) walker exercises. Parallel to
// fakeAzureSQL — the AKS-only test matrix runs against a focused
// mock that also routes the token / VM / SQL-server endpoints with
// minimal default responses so end-to-end "all three walked" tests
// run against a single fake.

type fakeAzureAKS struct {
	mu sync.Mutex

	// Clusters / VMs / Servers seed the per-surface list responses
	// the three walkers traverse. Empty when a test does not
	// exercise that surface.
	Clusters []armAKSCluster
	VMs      []armVirtualMachine
	Servers  []armSQLServer

	// AKSListStatus / AKSListErrorCode / AKSListRetryAfter drive
	// the failure branches on the AKS list endpoint: non-zero
	// status returns an armErrorResponse with the (defaulted) code
	// and the optional Retry-After header (set to non-empty to
	// exercise rate-limit independently of 429).
	AKSListStatus     int
	AKSListErrorCode  string
	AKSListRetryAfter string

	// Call counters and last-bearer capture for assertions.
	TokenCalls   int
	VMListCalls  int
	SQLListCalls int
	AKSListCalls int
	LastBearer   string
}

func newFakeAzureAKS() *fakeAzureAKS {
	return &fakeAzureAKS{}
}

func (f *fakeAzureAKS) handler() http.Handler {
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
			f.SQLListCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armSQLServerListResponse{Value: f.Servers})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.ContainerService/managedClusters"):
			f.AKSListCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.AKSListRetryAfter != "" {
				w.Header().Set("Retry-After", f.AKSListRetryAfter)
			}
			if f.AKSListStatus != 0 {
				code := f.AKSListErrorCode
				if code == "" {
					code = armErrorCodeFor(f.AKSListStatus)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.AKSListStatus)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{Code: code, Message: armErrorMessageFor(code)},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armAKSListResponse{Value: f.Clusters})
			return

		case strings.HasSuffix(path, "/providers/Microsoft.Web/sites"):
			// Serverless-tier-slice-1 (chunk 3, v0.89.91, #723
			// Stream 121): the AKS-walk fake also routes the
			// Microsoft.Web/sites list endpoint so existing AKS
			// tests don't get spurious "azfunc" partial failures
			// from the chunk-3 serverless walker that always runs
			// after the AKS walk. The default response is an empty
			// sites list.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armWebSiteListResponse{Value: nil})
			return
		}

		// Object-store + load-balancer tiers run after the AKS walk;
		// route them empty so AKS tests don't get spurious
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

func newAKSScannerWithFake(t *testing.T, fake *fakeAzureAKS) *Scanner {
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

// makeAKSCluster constructs an armAKSCluster with the slice-2
// detection-rule-relevant fields plumbed through. The three
// observability axes are independently configurable so the test
// matrix can flip one at a time.
func makeAKSCluster(name, location, kubeVersion, provisioningState, powerCode string, tags map[string]string, omsEnabled bool, hasOMS bool, monitorMetricsEnabled *bool, monitorContainerInsightsEnabled *bool, monitorProfilePresent bool) armAKSCluster {
	props := armAKSProperties{
		KubernetesVersion: kubeVersion,
		ProvisioningState: provisioningState,
	}
	if powerCode != "" {
		props.PowerState = &armAKSPowerState{Code: powerCode}
	}
	if hasOMS {
		props.AddonProfiles = map[string]armAKSAddon{
			aksOMSAgentAddonName: {Enabled: omsEnabled},
		}
	}
	if monitorProfilePresent {
		profile := &armAKSAzureMonitorProfile{}
		if monitorMetricsEnabled != nil {
			profile.Metrics = &armAKSMonitorFlag{Enabled: *monitorMetricsEnabled}
		}
		if monitorContainerInsightsEnabled != nil {
			profile.ContainerInsights = &armAKSMonitorFlag{Enabled: *monitorContainerInsightsEnabled}
		}
		props.AzureMonitorProfile = profile
	}
	return armAKSCluster{
		ID:         fmt.Sprintf("/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-%s/providers/Microsoft.ContainerService/managedClusters/%s", name, name),
		Name:       name,
		Location:   location,
		Tags:       tags,
		Properties: props,
	}
}

// boolp returns a pointer to b so test cases can distinguish
// "metrics.enabled present and false" from "metrics block absent".
func boolp(b bool) *bool { return &b }

// --- Tests ------------------------------------------------------------

func TestScan_AKS_ReturnsClusterSnapshot(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks-prod", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, map[string]string{"env": "prod"}, false, true, nil, nil, false),
		makeAKSCluster("aks-stage", "westus2", "1.28.7", aksProvisioningSucceeded, aksRunningPowerState, nil, false, true, nil, nil, false),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 2, "expected 2 cluster snapshot entries")
	assert.Equal(t, 1, fake.AKSListCalls)
	assert.Equal(t, "Bearer fake-bearer-token", fake.LastBearer)
	assert.False(t, res.Partial)
	assert.Empty(t, res.FailedServices)

	byName := map[string]int{}
	for i, c := range res.Clusters {
		byName[c.Name] = i
	}
	require.Contains(t, byName, "aks-prod")
	require.Contains(t, byName, "aks-stage")

	prod := res.Clusters[byName["aks-prod"]]
	assert.Equal(t, "1.29", prod.KubernetesVersion, "version reduced to major.minor")
	assert.Equal(t, "RUNNING", prod.Status)
	assert.Equal(t, "eastus", prod.Region)
	assert.Equal(t, map[string]string{"env": "prod"}, prod.Tags)
	assert.Contains(t, prod.ResourceID, "managedClusters/aks-prod")

	stage := res.Clusters[byName["aks-stage"]]
	assert.Equal(t, "1.28", stage.KubernetesVersion)
	assert.Equal(t, "westus2", stage.Region)
	assert.Nil(t, stage.Tags)
}

func TestScan_AKS_OMSAgentEnabled_DetectsHasMonitor(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil,
			true, // omsEnabled
			true, // hasOMS profile entry
			nil, nil, false),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.True(t, res.Clusters[0].AzureMonitorEnabled, "omsagent.enabled=true must flip AzureMonitorEnabled")
}

func TestScan_AKS_AzureMonitorMetricsEnabled_DetectsHasMonitor(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil,
			false, false,
			boolp(true), // azureMonitorProfile.metrics.enabled
			nil,
			true),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.True(t, res.Clusters[0].AzureMonitorEnabled, "azureMonitorProfile.metrics.enabled=true must flip AzureMonitorEnabled")
}

func TestScan_AKS_ContainerInsightsEnabled_DetectsHasMonitor(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil,
			false, false,
			nil,
			boolp(true), // azureMonitorProfile.containerInsights.enabled
			true),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.True(t, res.Clusters[0].AzureMonitorEnabled, "azureMonitorProfile.containerInsights.enabled=true must flip AzureMonitorEnabled")
}

func TestScan_AKS_AllThreeDisabled_DetectsAsFalse(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil,
			false, true, // omsagent profile present, enabled=false
			boolp(false), // metrics.enabled=false
			boolp(false), // containerInsights.enabled=false
			true),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].AzureMonitorEnabled, "all three flags false → AzureMonitorEnabled=false")
}

func TestScan_AKS_NoAddonProfiles_TreatsAsFalse(t *testing.T) {
	// addonProfiles entirely absent — falls through to
	// azureMonitorProfile checks; both absent here too, so the
	// detection rule terminates at false. The defensive nil
	// guards in hasAzureMonitor must not panic.
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil,
			false, false, // hasOMS=false → no addonProfiles map
			nil, nil, false), // monitorProfilePresent=false
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].AzureMonitorEnabled)
}

func TestScan_AKS_NoAzureMonitorProfile_TreatsAsFalse(t *testing.T) {
	// azureMonitorProfile entirely absent — relies solely on the
	// omsagent legacy addon. With omsagent.enabled=false, the
	// disjunction terminates at false rather than panicking on
	// the missing azureMonitorProfile pointer.
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil,
			false, true, // omsagent profile present, enabled=false
			nil, nil, false), // no azureMonitorProfile
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].AzureMonitorEnabled)
}

func TestScan_AKS_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.AKSListStatus = http.StatusForbidden
	fake.AKSListErrorCode = "AuthorizationFailed"

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied at AKS list is a partial-failure surface")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "Reader role")
	assert.Contains(t, res.FailedServices, ServiceIDAKS)
	assert.Empty(t, res.Clusters)
}

func TestScan_AKS_RateLimited_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.AKSListStatus = http.StatusTooManyRequests

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDAKS)
	assert.Empty(t, res.Clusters)
}

func TestScan_AKS_EmptyClusterList_NoPartialFailure(t *testing.T) {
	fake := newFakeAzureAKS()
	// fake.Clusters stays nil — endpoint returns Value: [].

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.False(t, res.Partial, "empty AKS inventory is NOT a partial failure")
	assert.Empty(t, res.PartialReason)
	assert.NotContains(t, res.FailedServices, ServiceIDAKS)
	assert.Empty(t, res.Clusters)
	assert.Equal(t, 1, fake.AKSListCalls, "the endpoint was still hit")
}

func TestScan_AKS_ProviderFieldSet(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks-a", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil, true, true, nil, nil, false),
		makeAKSCluster("aks-b", "westus2", "1.28.7", aksProvisioningSucceeded, aksRunningPowerState, nil, false, false, boolp(true), nil, true),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 2)
	for _, c := range res.Clusters {
		assert.Equal(t, "azure", c.Provider, "every snapshot Provider field must be azure")
	}
}

func TestScan_VMsSQLAndAKS_AllThreeWalked(t *testing.T) {
	fake := newFakeAzureAKS()
	fake.VMs = []armVirtualMachine{
		makeVM("web-1", "eastus", "Standard_D4s_v3", "Linux", map[string]string{"otel": "v1"}),
	}
	fake.Servers = []armSQLServer{makeSQLServer("sqlsrv", "eastus")}
	fake.Clusters = []armAKSCluster{
		makeAKSCluster("aks-prod", "eastus", "1.29.4", aksProvisioningSucceeded, aksRunningPowerState, nil, true, true, nil, nil, false),
	}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 1)
	// SQL servers seeded but no databases plumbed via this fake
	// (it short-circuits to default — no /databases handler), so
	// the SQL walk records a partial failure on the per-server
	// database list. That is unrelated to the AKS assertion below;
	// what matters is that the AKS walk runs to completion AND
	// the VM walk runs to completion regardless of the SQL state.
	require.Len(t, res.Clusters, 1)
	assert.True(t, res.Compute[0].HasOTel)
	assert.True(t, res.Clusters[0].AzureMonitorEnabled)
	assert.Equal(t, 1, fake.VMListCalls)
	assert.Equal(t, 1, fake.SQLListCalls)
	assert.Equal(t, 1, fake.AKSListCalls)
}

// --- Detection-rule unit tests ---------------------------------------
//
// Independent of the HTTP-mock test matrix, the three-way disjunction
// is pure-function. Pin every combination so a future refactor that
// reorders the checks or swaps the short-circuit can't regress the
// rule silently.

func TestHasAzureMonitor_DirectMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   armAKSProperties
		want bool
	}{
		{
			name: "all absent",
			in:   armAKSProperties{},
			want: false,
		},
		{
			name: "omsagent enabled true",
			in: armAKSProperties{
				AddonProfiles: map[string]armAKSAddon{
					aksOMSAgentAddonName: {Enabled: true},
				},
			},
			want: true,
		},
		{
			name: "omsagent enabled false, no monitor profile",
			in: armAKSProperties{
				AddonProfiles: map[string]armAKSAddon{
					aksOMSAgentAddonName: {Enabled: false},
				},
			},
			want: false,
		},
		{
			name: "metrics.enabled true",
			in: armAKSProperties{
				AzureMonitorProfile: &armAKSAzureMonitorProfile{
					Metrics: &armAKSMonitorFlag{Enabled: true},
				},
			},
			want: true,
		},
		{
			name: "containerInsights.enabled true",
			in: armAKSProperties{
				AzureMonitorProfile: &armAKSAzureMonitorProfile{
					ContainerInsights: &armAKSMonitorFlag{Enabled: true},
				},
			},
			want: true,
		},
		{
			name: "monitor profile present but both nested nil",
			in: armAKSProperties{
				AzureMonitorProfile: &armAKSAzureMonitorProfile{},
			},
			want: false,
		},
		{
			name: "addonProfiles present without omsagent key",
			in: armAKSProperties{
				AddonProfiles: map[string]armAKSAddon{
					"httpApplicationRouting": {Enabled: true},
				},
			},
			want: false,
		},
		{
			name: "omsagent false but metrics true wins via disjunction",
			in: armAKSProperties{
				AddonProfiles: map[string]armAKSAddon{
					aksOMSAgentAddonName: {Enabled: false},
				},
				AzureMonitorProfile: &armAKSAzureMonitorProfile{
					Metrics: &armAKSMonitorFlag{Enabled: true},
				},
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasAzureMonitor(tc.in))
		})
	}
}

func TestNormalizeAKSStatus_DirectCases(t *testing.T) {
	cases := []struct {
		name      string
		provState string
		powerCode string
		hasPower  bool
		want      string
	}{
		{"succeeded + running → RUNNING", "Succeeded", "Running", true, "RUNNING"},
		{"succeeded + stopped → Succeeded raw", "Succeeded", "Stopped", true, "Succeeded"},
		{"updating → raw", "Updating", "Running", true, "Updating"},
		{"failed → raw", "Failed", "Running", true, "Failed"},
		{"creating → raw", "Creating", "", false, "Creating"},
		{"empty + no power → UNKNOWN", "", "", false, "UNKNOWN"},
		{"succeeded + no power block → Succeeded raw", "Succeeded", "", false, "Succeeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			props := armAKSProperties{ProvisioningState: tc.provState}
			if tc.hasPower {
				props.PowerState = &armAKSPowerState{Code: tc.powerCode}
			}
			assert.Equal(t, tc.want, normalizeAKSStatus(props))
		})
	}
}

func TestExtractMajorMinor_DirectCases(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"1.29.4", "1.29"},
		{"1.29", "1.29"},
		{"1.30.0-rc.0", "1.30"},
		{"", ""},
		{"x", "x"},
		{"v1.29.4", "v1.29"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.out, extractMajorMinor(tc.in), "in=%q", tc.in)
	}
}

func TestScan_AKS_FallsBackToCurrentKubernetesVersion(t *testing.T) {
	// Mid-creation cluster: kubernetesVersion has not yet been
	// populated; currentKubernetesVersion carries the running
	// version. The picker should fall back rather than emit empty.
	fake := newFakeAzureAKS()
	cluster := makeAKSCluster("aks", "eastus", "", aksProvisioningSucceeded, aksRunningPowerState, nil, false, false, nil, nil, false)
	cluster.Properties.CurrentKubernetesVersion = "1.27.9"
	fake.Clusters = []armAKSCluster{cluster}

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 1)
	assert.Equal(t, "1.27", res.Clusters[0].KubernetesVersion)
}

func TestScan_AKS_NetworkError_RecordsPartialFailure(t *testing.T) {
	// Defense-in-depth for the network branch of classifyAKSError:
	// 503 + Retry-After → rate_limit (covered above), but a true
	// network error (non-HTTP) should surface a "network error"
	// reason. Simulated here by setting AKSListStatus to a 5xx
	// without Retry-After.
	fake := newFakeAzureAKS()
	fake.AKSListStatus = http.StatusInternalServerError

	s := newAKSScannerWithFake(t, fake)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDAKS)
	assert.Contains(t, res.FailedServices, ServiceIDAKS)
}
