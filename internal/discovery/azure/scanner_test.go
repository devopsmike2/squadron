// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// --- Test doubles -----------------------------------------------------
//
// fakeAzure is an httptest-backed mock of the Azure AD token endpoint
// and the Azure Resource Manager VM list endpoint. It only implements
// the two routes slice 1 walks, which is enough to exercise every
// code path in scanner.go without standing up real Azure credentials.
//
// Tests seed VMs with the response shape they want; the mock
// dispatches based on URL path. Failure tests set the matching
// *Status field so the next call to that route returns the configured
// status code instead of a successful response.

type fakeAzure struct {
	mu sync.Mutex

	// VMs is the static VM list served by the ARM list endpoint.
	VMs []armVirtualMachine

	// TokenStatus, when non-zero, makes the next token call return
	// this status (with the token error JSON body shape).
	TokenStatus int

	// TokenErrorBody is the JSON body returned on a non-200 token
	// call. Defaults to {"error":"invalid_client","error_description":"…"}.
	TokenErrorBody armTokenError

	// ARMListStatus, when non-zero, makes the next ARM list call
	// return this status (with the armErrorResponse body shape).
	ARMListStatus int

	// ARMListErrorCode is the .error.code field returned on a
	// non-200 ARM list call. Defaults to a generic value matching
	// the configured status code.
	ARMListErrorCode string

	// ARMListRetryAfter is the optional Retry-After response header
	// for ARM list calls. Tests set this to non-empty to exercise
	// the rate-limit branch independently of the 429 status.
	ARMListRetryAfter string

	// Call counters for assertions.
	TokenCalls   int
	ARMListCalls int

	// Captured request headers for assertion.
	LastTokenForm map[string]string
	LastBearer    string
}

func newFakeAzure() *fakeAzure {
	return &fakeAzure{}
}

func (f *fakeAzure) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			f.TokenCalls++
			_ = r.ParseForm()
			f.LastTokenForm = map[string]string{}
			for k, v := range r.PostForm {
				if len(v) > 0 {
					f.LastTokenForm[k] = v[0]
				}
			}
			if f.TokenStatus != 0 {
				body := f.TokenErrorBody
				if body.Error == "" {
					body.Error = "invalid_client"
					body.ErrorDescription = "AADSTS70002: configured fake token error"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.TokenStatus)
				_ = json.NewEncoder(w).Encode(body)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armTokenResponse{
				AccessToken: "fake-bearer-token",
				TokenType:   "Bearer",
				ExpiresIn:   3600,
			})
			return

		case strings.HasSuffix(r.URL.Path, "/providers/Microsoft.Compute/virtualMachines"):
			f.ARMListCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.ARMListRetryAfter != "" {
				w.Header().Set("Retry-After", f.ARMListRetryAfter)
			}
			if f.ARMListStatus != 0 {
				code := f.ARMListErrorCode
				if code == "" {
					code = armErrorCodeFor(f.ARMListStatus)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.ARMListStatus)
				_ = json.NewEncoder(w).Encode(armErrorResponse{
					Error: armErrorBody{
						Code:    code,
						Message: armErrorMessageFor(code),
					},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armVMListResponse{Value: f.VMs})
			return

		case strings.HasSuffix(r.URL.Path, "/providers/Microsoft.Sql/servers"):
			// Slice-2 chunk-3: the VM-walk fakeAzure also routes the
			// SQL server list endpoint so slice-1 VM tests don't get
			// spurious "azuresql" partial failures from the new
			// chunk-3 walker that always runs after the VM walk.
			// The default response is an empty server list —
			// operator has no SQL inventory, the walker appends zero
			// database snapshots and records no partial failure.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armSQLServerListResponse{Value: nil})
			return

		case strings.HasSuffix(r.URL.Path, "/providers/Microsoft.ContainerService/managedClusters"):
			// Kubernetes-tier-slice-2 (chunk 3): the VM-walk
			// fakeAzure also routes the AKS managedClusters list
			// endpoint so slice-1 VM tests and slice-2 SQL tests
			// don't get spurious "aks" partial failures from the
			// new chunk-3 walker that always runs after the VM and
			// SQL walks. The default response is an empty cluster
			// list — operator has no AKS inventory, the walker
			// appends zero ClusterSnapshot entries and records no
			// partial failure.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(armAKSListResponse{Value: nil})
			return
		}

		// Unmatched path — surface as 404 so test failures are
		// obvious rather than the scanner silently consuming an
		// empty body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(armErrorResponse{
			Error: armErrorBody{
				Code:    "NotFound",
				Message: fmt.Sprintf("unhandled mock path: %s", r.URL.Path),
			},
		})
	})
}

func armErrorCodeFor(code int) string {
	switch code {
	case http.StatusForbidden:
		return "AuthorizationFailed"
	case http.StatusNotFound:
		return "SubscriptionNotFound"
	case http.StatusUnauthorized:
		return "InvalidAuthenticationToken"
	case http.StatusTooManyRequests:
		return "TooManyRequests"
	default:
		return "InternalServerError"
	}
}

func armErrorMessageFor(code string) string {
	switch code {
	case "AuthorizationFailed":
		return "The client does not have authorization to perform action 'Microsoft.Compute/virtualMachines/read'."
	case "SubscriptionNotFound":
		return "The subscription could not be found."
	case "InvalidAuthenticationToken":
		return "The access token is invalid."
	case "TooManyRequests":
		return "Rate limit exceeded."
	default:
		return "Internal server error."
	}
}

// newScannerWithFake wires a Scanner against the supplied fake's
// httptest server. The test takes ownership of cleanup via t.Cleanup.
func newScannerWithFake(t *testing.T, fake *fakeAzure, location string) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		Location:       location,
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}
}

// --- Helpers for VM construction --------------------------------------

func makeVM(name, location, vmSize, osType string, tags map[string]string) armVirtualMachine {
	return armVirtualMachine{
		ID:       fmt.Sprintf("/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-%s/providers/Microsoft.Compute/virtualMachines/%s", name, name),
		Name:     name,
		Location: location,
		Tags:     tags,
		Properties: armVirtualMachineP{
			HardwareProfile: armHardwareProfile{VMSize: vmSize},
			StorageProfile:  armStorageProfile{OSDisk: armOSDisk{OSType: osType}},
		},
	}
}

// --- Tests ------------------------------------------------------------

func TestScan_ReturnsVMsWithComputeInstanceSnapshotShape(t *testing.T) {
	fake := newFakeAzure()
	fake.VMs = []armVirtualMachine{
		makeVM("web-1", "eastus", "Standard_D4s_v3", "Linux", map[string]string{"env": "prod"}),
		makeVM("web-2", "eastus", "Standard_B2ms", "Linux", map[string]string{"otel-collector": "v1"}),
		makeVM("worker-1", "westus2", "Standard_D2s_v3", "Windows", nil),
	}

	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 3, "expected 3 snapshot entries across both regions")
	assert.Equal(t, credstore.ProviderAzure, res.Provider)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", res.AccountID)
	assert.False(t, res.Partial)
	assert.Empty(t, res.PartialReason)
	assert.Empty(t, res.FailedServices)
	assert.NotEmpty(t, res.ScanID)
	assert.False(t, res.ScanStartedAt.IsZero())
	assert.False(t, res.ScanCompletedAt.IsZero())

	// Confirm the OAuth flow ran with the correct form values.
	assert.Equal(t, 1, fake.TokenCalls)
	assert.Equal(t, "client_credentials", fake.LastTokenForm["grant_type"])
	assert.Equal(t, "33333333-3333-3333-3333-333333333333", fake.LastTokenForm["client_id"])
	assert.Equal(t, "super-secret", fake.LastTokenForm["client_secret"])
	assert.Equal(t, armScope, fake.LastTokenForm["scope"])

	// Confirm the ARM call carried the Bearer token.
	assert.Equal(t, "Bearer fake-bearer-token", fake.LastBearer)

	byID := map[string]int{}
	for i, c := range res.Compute {
		byID[c.ResourceID] = i
	}
	require.Contains(t, byID, "web-1")
	require.Contains(t, byID, "web-2")
	require.Contains(t, byID, "worker-1")

	web1 := res.Compute[byID["web-1"]]
	assert.Equal(t, "Standard_D4s_v3", web1.InstanceType)
	assert.Equal(t, "eastus", web1.Region)
	assert.Equal(t, "linux", web1.OSFamily)
	assert.Equal(t, map[string]string{"env": "prod"}, web1.Tags)
	assert.False(t, web1.HasOTel)

	web2 := res.Compute[byID["web-2"]]
	assert.True(t, web2.HasOTel)
	assert.Equal(t, "linux", web2.OSFamily)

	worker := res.Compute[byID["worker-1"]]
	assert.Equal(t, "westus2", worker.Region)
	assert.Equal(t, "windows", worker.OSFamily)
	assert.Nil(t, worker.Tags)

	sort.Strings(res.Regions)
	assert.Equal(t, []string{"eastus", "westus2"}, res.Regions)
}

func TestScan_HasOTelTrueForOtelTag(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"lowercase otel prefix", map[string]string{"otel": "v1"}},
		{"otel-collector compound", map[string]string{"otel-collector": "v1", "env": "prod"}},
		{"OTEL uppercase prefix", map[string]string{"OTEL_AGENT": "v1"}},
		{"mixed-case", map[string]string{"Otel": "v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAzure()
			fake.VMs = []armVirtualMachine{makeVM("inst", "eastus", "Standard_B2ms", "Linux", tc.tags)}
			s := newScannerWithFake(t, fake, "")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.True(t, res.Compute[0].HasOTel, "expected HasOTel=true for tags %v", tc.tags)
			assert.Equal(t, 1, res.InstrumentedCount)
			assert.Equal(t, 0, res.UninstrumentedCount)
		})
	}
}

func TestScan_HasOTelFalseForNoOtelTag(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"no tags", nil},
		{"empty tags", map[string]string{}},
		{"non-otel tags", map[string]string{"env": "prod", "team": "platform"}},
		{"close-but-not-prefix", map[string]string{"telemetry": "on", "monitoring": "yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAzure()
			fake.VMs = []armVirtualMachine{makeVM("inst", "eastus", "Standard_B2ms", "Linux", tc.tags)}
			s := newScannerWithFake(t, fake, "")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.False(t, res.Compute[0].HasOTel, "expected HasOTel=false for tags %v", tc.tags)
			assert.Equal(t, 0, res.InstrumentedCount)
			assert.Equal(t, 1, res.UninstrumentedCount)
		})
	}
}

func TestScan_OSFamilyFromOSType_Linux(t *testing.T) {
	fake := newFakeAzure()
	fake.VMs = []armVirtualMachine{makeVM("inst", "eastus", "Standard_B2ms", "Linux", nil)}
	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Compute, 1)
	assert.Equal(t, "linux", res.Compute[0].OSFamily)
}

func TestScan_OSFamilyFromOSType_Windows(t *testing.T) {
	fake := newFakeAzure()
	fake.VMs = []armVirtualMachine{makeVM("inst", "eastus", "Standard_B2ms", "Windows", nil)}
	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Compute, 1)
	assert.Equal(t, "windows", res.Compute[0].OSFamily)
}

func TestScan_OSFamilyUnknownForMissingOSType(t *testing.T) {
	fake := newFakeAzure()
	fake.VMs = []armVirtualMachine{makeVM("inst", "eastus", "Standard_B2ms", "", nil)}
	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Compute, 1)
	assert.Equal(t, "unknown", res.Compute[0].OSFamily)
}

func TestScan_LocationFilterRestrictsRegions(t *testing.T) {
	fake := newFakeAzure()
	fake.VMs = []armVirtualMachine{
		makeVM("east-1", "eastus", "Standard_B2ms", "Linux", nil),
		makeVM("east-2", "eastus", "Standard_B2ms", "Linux", nil),
		makeVM("west-1", "westus2", "Standard_B2ms", "Linux", nil),
	}
	s := newScannerWithFake(t, fake, "eastus")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 2)
	ids := map[string]struct{}{}
	for _, c := range res.Compute {
		ids[c.ResourceID] = struct{}{}
		assert.Equal(t, "eastus", c.Region)
	}
	assert.Contains(t, ids, "east-1")
	assert.Contains(t, ids, "east-2")
	assert.NotContains(t, ids, "west-1")

	assert.Equal(t, []string{"eastus"}, res.Regions)
}

func TestScan_RateLimit_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzure()
	fake.ARMListStatus = http.StatusTooManyRequests

	s := newScannerWithFake(t, fake, "eastus")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "partial failures return nil error")

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDVirtualMachines)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDVirtualMachines)
	assert.Empty(t, res.Compute)
	assert.Equal(t, []string{"eastus"}, res.Regions, "configured location preserved on partial")
}

func TestScan_RateLimit_RetryAfterHeader_RecordsPartialFailure(t *testing.T) {
	// Defense-in-depth: ARM occasionally returns 503 + Retry-After
	// under throttling. The classifier should still surface a
	// rate-limit reason rather than the generic 5xx tail.
	fake := newFakeAzure()
	fake.ARMListStatus = http.StatusServiceUnavailable
	fake.ARMListRetryAfter = "30"

	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDVirtualMachines)
}

func TestScan_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzure()
	fake.ARMListStatus = http.StatusForbidden
	fake.ARMListErrorCode = "AuthorizationFailed"

	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied at VM list is a partial-failure surface")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "Reader role")
	assert.Contains(t, res.FailedServices, ServiceIDVirtualMachines)
	assert.Empty(t, res.Compute)
}

func TestScan_SubscriptionNotFound_RecordsPartialFailure(t *testing.T) {
	fake := newFakeAzure()
	fake.ARMListStatus = http.StatusNotFound
	fake.ARMListErrorCode = "SubscriptionNotFound"

	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "subscription not found")
	assert.Contains(t, res.PartialReason, "subscription_id")
	assert.Contains(t, res.FailedServices, ServiceIDVirtualMachines)
	assert.Empty(t, res.Compute)
}

func TestScan_CredentialsInvalid_TokenEndpointReturns401(t *testing.T) {
	fake := newFakeAzure()
	fake.TokenStatus = http.StatusUnauthorized
	fake.TokenErrorBody = armTokenError{
		Error:            "invalid_client",
		ErrorDescription: "AADSTS7000215: Invalid client secret provided.",
	}

	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.Error(t, err, "token endpoint auth failure is a hard error")
	assert.Contains(t, err.Error(), ServiceIDVirtualMachines)
	assert.Contains(t, err.Error(), "token endpoint auth failed")
	assert.NotContains(t, err.Error(), "super-secret", "client_secret never echoed in errors")

	// Result fields that ARE expected to be populated on hard
	// failure: ScanID, ScanStartedAt, ScanCompletedAt, Provider,
	// AccountID. Compute / Regions stay empty.
	assert.Equal(t, credstore.ProviderAzure, res.Provider)
	assert.NotEmpty(t, res.ScanID)
	assert.Empty(t, res.Compute)
	// 0 ARM calls because the token failure short-circuits.
	assert.Equal(t, 0, fake.ARMListCalls)
}

func TestScan_NetworkError_RecordsPartialFailure(t *testing.T) {
	// Open a token-only mock so the token call succeeds, then
	// have the ARM endpoint point at a dead address. The token
	// path runs against a live httptest server; the ARM path
	// against a closed listener.
	tokenFake := newFakeAzure()
	tokenSrv := httptest.NewServer(tokenFake.handler())
	t.Cleanup(tokenSrv.Close)

	// Pick a free port, close it, and point armEndpoint at the
	// dead address.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	s := &Scanner{
		TenantID:       "t",
		SubscriptionID: "sub",
		ClientID:       "c",
		ClientSecret:   []byte("x"),
		httpClient:     &http.Client{Timeout: 2 * time.Second},
		armEndpoint:    "http://" + addr,
		tokenEndpoint:  tokenSrv.URL,
	}
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "transport failures at the ARM layer are partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "network")
	assert.Contains(t, res.FailedServices, ServiceIDVirtualMachines)
}

func TestScan_TokenNetworkError_ReturnsHardError(t *testing.T) {
	// Token endpoint at a dead address — the OAuth call itself
	// fails at the transport layer, which is the substrate-level
	// hard failure surface.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	s := &Scanner{
		TenantID:       "t",
		SubscriptionID: "sub",
		ClientID:       "c",
		ClientSecret:   []byte("x"),
		httpClient:     &http.Client{Timeout: 2 * time.Second},
		tokenEndpoint:  "http://" + addr,
	}
	res, err := s.Scan(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), ServiceIDVirtualMachines)
	assert.Contains(t, strings.ToLower(err.Error()), "network")
	// Still scan-bracketed on hard failure.
	assert.NotEmpty(t, res.ScanID)
	assert.False(t, res.ScanStartedAt.IsZero())
	assert.False(t, res.ScanCompletedAt.IsZero())
}

func TestScan_InstrumentedCountMatchesHasOTelTrue(t *testing.T) {
	fake := newFakeAzure()
	fake.VMs = []armVirtualMachine{
		makeVM("a", "eastus", "Standard_B2ms", "Linux", map[string]string{"otel": "v1"}),
		makeVM("b", "eastus", "Standard_B2ms", "Linux", map[string]string{"otel-collector": "v1"}),
		makeVM("c", "eastus", "Standard_B2ms", "Linux", map[string]string{"env": "prod"}),
		makeVM("d", "eastus", "Standard_B2ms", "Linux", nil),
		makeVM("e", "eastus", "Standard_B2ms", "Linux", map[string]string{"team": "data"}),
	}
	s := newScannerWithFake(t, fake, "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Compute, 5)
	assert.Equal(t, 2, res.InstrumentedCount)
	assert.Equal(t, 3, res.UninstrumentedCount)
}

func TestScan_RequiresSubscriptionID(t *testing.T) {
	s := &Scanner{
		TenantID:     "t",
		ClientID:     "c",
		ClientSecret: []byte("x"),
	}
	_, err := s.Scan(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SubscriptionID")
}

func TestScan_RequiresAuth(t *testing.T) {
	cases := []struct {
		name string
		s    *Scanner
	}{
		{"missing TenantID", &Scanner{SubscriptionID: "sub", ClientID: "c", ClientSecret: []byte("x")}},
		{"missing ClientID", &Scanner{SubscriptionID: "sub", TenantID: "t", ClientSecret: []byte("x")}},
		{"missing ClientSecret", &Scanner{SubscriptionID: "sub", TenantID: "t", ClientID: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.s.Scan(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "TenantID, ClientID, and ClientSecret")
		})
	}
}

func TestProvider_ReturnsAzure(t *testing.T) {
	s := &Scanner{}
	assert.Equal(t, credstore.ProviderAzure, s.Provider())
}

func TestNormalizeOSType_Cases(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"Linux", "linux"},
		{"linux", "linux"},
		{"LINUX", "linux"},
		{"Windows", "windows"},
		{"windows", "windows"},
		{"WINDOWS", "windows"},
		{"", "unknown"},
		{"FreeBSD", "unknown"},
		{"   ", "unknown"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.out, normalizeOSType(tc.in), "in=%q", tc.in)
	}
}

func TestHasOTelTag_DirectCases(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"nil map", nil, false},
		{"empty map", map[string]string{}, false},
		{"single otel key", map[string]string{"otel": "v"}, true},
		{"otel prefix mixed case", map[string]string{"OtelCollector": "v"}, true},
		{"otel-suffixed key matches prefix", map[string]string{"OTEL_AGENT_VERSION": "1"}, true},
		{"telemetry is not otel", map[string]string{"telemetry": "on"}, false},
		{"otel buried mid-string does not match", map[string]string{"env-otel-prod": "v"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasOTelTag(tc.tags))
		})
	}
}

// TestScan_BodyHintTruncation pins the truncate cap so audit payloads
// don't bloat when ARM returns a multi-kilobyte error body.
func TestScan_BodyHintTruncation(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncate(long, 200)
	assert.Equal(t, 203, len(got))
	assert.True(t, strings.HasSuffix(got, "..."))
}
