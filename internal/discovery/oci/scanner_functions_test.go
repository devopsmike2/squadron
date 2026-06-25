// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// --- Helpers: OCI Functions httptest mock ----------------------------
//
// fakeOCIFunctions is a slim httptest-backed mock targeted at the
// chunk-4 OCI Functions tests. It deliberately mirrors the existing
// fakeOCI scaffolding (see scanner_test.go) but only routes the
// endpoints chunk 4 needs — /compartments + /applications +
// /functions — so the chunk-4 tests don't depend on the compute /
// database / OKE fields the older fake exposes. Keeping it separate
// also keeps the chunk-1 compute / chunk-2 database / OKE tests in
// scanner_test.go untouched per the chunk-4 brief's "DO NOT modify
// existing OCI scanner tests" constraint.

type fakeOCIFunctions struct {
	mu sync.Mutex

	// Compartments returned by the identity list call. Empty means
	// the call returns an empty array (the scanner falls back to
	// scanning only the tenancy root).
	Compartments []ociCompartment

	// ApplicationsByCompartment maps compartmentId -> Applications
	// served when /applications is called with that compartmentId.
	// Missing compartmentId returns an empty list (not a 404). When
	// AppsByPage is non-empty for a compartment, the mock prefers
	// the pagination shape (see PaginationCompartment / AppsByPage).
	ApplicationsByCompartment map[string][]ociApplication

	// FunctionsByApplication maps applicationId -> Functions served
	// when /functions is called with that applicationId. Missing
	// applicationId returns an empty list.
	FunctionsByApplication map[string][]ociFunction

	// PaginationCompartment, when set, opts the named compartment
	// into a multi-page /applications response. The mock then
	// returns AppsByPage[""] on the first call, AppsByPage["next"]
	// on the page=next call, etc. The opc-next-page header carries
	// the token for the subsequent page; the final page emits no
	// header.
	PaginationCompartment string

	// AppsByPage holds the per-page application list for the
	// PaginationCompartment opt-in flow. The key is the page token
	// (empty string for the first page); the value is the page's
	// applications. NextPageByPage maps each page token to the
	// next-page token to set on the response header.
	AppsByPage     map[string][]ociApplication
	NextPageByPage map[string]string

	// Call counters.
	CompartmentsCalls int
	ApplicationsCalls int
	FunctionsCalls    int

	// Per-endpoint failure switches. Status==0 means "no failure
	// configured"; any non-zero status returns that code with a
	// JSON error body.
	ApplicationsStatus int
	FunctionsStatus    int
	CompartmentsStatus int
}

func newFakeOCIFunctions() *fakeOCIFunctions {
	return &fakeOCIFunctions{
		ApplicationsByCompartment: map[string][]ociApplication{},
		FunctionsByApplication:    map[string][]ociFunction{},
		AppsByPage:                map[string][]ociApplication{},
		NextPageByPage:            map[string]string{},
	}
}

func (f *fakeOCIFunctions) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/compartments"):
			f.CompartmentsCalls++
			if f.CompartmentsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.CompartmentsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			comps := f.Compartments
			if comps == nil {
				comps = []ociCompartment{}
			}
			_ = json.NewEncoder(w).Encode(comps)
			return

		case strings.HasSuffix(r.URL.Path, "/applications"):
			f.ApplicationsCalls++
			compartmentID := r.URL.Query().Get("compartmentId")
			pageToken := r.URL.Query().Get("page")

			if f.ApplicationsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.ApplicationsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}

			// Pagination opt-in path: when this compartment matches
			// PaginationCompartment, serve AppsByPage[token] and set
			// the next-page header to NextPageByPage[token].
			if compartmentID == f.PaginationCompartment && f.PaginationCompartment != "" {
				apps := f.AppsByPage[pageToken]
				if apps == nil {
					apps = []ociApplication{}
				}
				if next := f.NextPageByPage[pageToken]; next != "" {
					w.Header().Set("opc-next-page", next)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(apps)
				return
			}

			apps := f.ApplicationsByCompartment[compartmentID]
			if apps == nil {
				apps = []ociApplication{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apps)
			return

		case strings.HasSuffix(r.URL.Path, "/functions"):
			f.FunctionsCalls++
			applicationID := r.URL.Query().Get("applicationId")

			if f.FunctionsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.FunctionsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}

			fns := f.FunctionsByApplication[applicationID]
			if fns == nil {
				fns = []ociFunction{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fns)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ociErrorBody{
			Code:    "NotFound",
			Message: "unhandled mock path: " + r.URL.Path,
		})
	})
}

// newFunctionsScannerWithFake wires a Scanner against the supplied
// fake's httptest server. Test takes ownership of cleanup via
// t.Cleanup. Mirrors newScannerWithFake (scanner_test.go) but
// targets the chunk-4 mock specifically.
func newFunctionsScannerWithFake(t *testing.T, fake *fakeOCIFunctions, region string) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	pemBytes, _ := generateTestKey(t)
	r := region
	if r == "" {
		r = "us-phoenix-1"
	}
	return &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		PrivateKey:  pemBytes,
		Region:      r,
		httpClient:  srv.Client(),
		ociEndpoint: srv.URL,
	}
}

// --- Fixture builders ------------------------------------------------

func makeOCIApplication(name string) ociApplication {
	return ociApplication{
		ID:             "ocid1.fnapp.oc1..." + name,
		DisplayName:    name,
		CompartmentID:  "ocid1.tenancy.oc1..aaa",
		LifecycleState: "ACTIVE",
	}
}

func makeOCIFunction(name, image string, config map[string]string) ociFunction {
	return ociFunction{
		ID:             "ocid1.fnfunc.oc1..." + name,
		DisplayName:    name,
		ApplicationID:  "ocid1.fnapp.oc1...app",
		CompartmentID:  "ocid1.tenancy.oc1..aaa",
		Image:          image,
		LifecycleState: "ACTIVE",
		Config:         config,
	}
}

// scopeRoot is the default ScanScope the chunk-4 tests use — empty
// AccountID + empty CompartmentIDs lets the scanner fall back to
// "tenancy root + first-level children" and stamp the configured
// TenancyOCID onto each snapshot's AccountID field.
func scopeRoot() scanner.ScanScope { return scanner.ScanScope{} }

// --- Test 9 (slice 1 §11): function with APM enabled flips
// HasTraceAxis to true. -------------------------------------------------

func TestOCIFunctionsScanner_FunctionWithAPMEnabled_HasTraceAxis(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		makeOCIApplication("prod-app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...prod-app"] = []ociFunction{
		makeOCIFunction("checkout", "iad.ocir.io/team/checkout:v1",
			map[string]string{OCIAPMEnabledConfigKey: "true"}),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasTraceAxis, "OCI_APM_ENABLED=true should flip HasTraceAxis")
	assert.False(t, snap.HasOTelDistro, "no OTEL_DISTRO config -> HasOTelDistro=false")
	assert.Equal(t, "oci", snap.Provider)
	assert.Equal(t, "ocifunc", snap.Surface)
	assert.Equal(t, "checkout", snap.ResourceName)
	assert.Equal(t, "ocid1.fnfunc.oc1...checkout", snap.ResourceARN)
	assert.Equal(t, "iad.ocir.io/team/checkout:v1", snap.Runtime)
	assert.Equal(t, "us-phoenix-1", snap.Region)
	assert.Equal(t, "ocid1.tenancy.oc1..aaa", snap.AccountID)
	assert.True(t, snap.IsInstrumented(), "HasTraceAxis alone carries IsInstrumented")
}

// --- Test: value other than "true" leaves HasTraceAxis false ---------

func TestOCIFunctionsScanner_FunctionWithAPMEnabledFalse_NoTraceAxis(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"literal_false", "false"},
		{"uppercase_TRUE", "TRUE"}, // exact-match-on-"true" — design doc canonical lowercase
		{"numeric_one", "1"},
		{"empty_string", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOCIFunctions()
			fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
				makeOCIApplication("app"),
			}
			fake.FunctionsByApplication["ocid1.fnapp.oc1...app"] = []ociFunction{
				makeOCIFunction("fn", "iad.ocir.io/team/fn:v1",
					map[string]string{OCIAPMEnabledConfigKey: tc.value}),
			}

			s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
			snaps, err := s.ScanServerless(context.Background(), scopeRoot())
			require.NoError(t, err)
			require.Len(t, snaps, 1)

			assert.False(t, snaps[0].HasTraceAxis,
				"non-canonical APM value %q should NOT flip HasTraceAxis", tc.value)
		})
	}
}

// --- Test: OTEL_DISTRO set flips HasOTelDistro -----------------------

func TestOCIFunctionsScanner_FunctionWithOTelDistro_HasOTelDistro(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		makeOCIApplication("app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...app"] = []ociFunction{
		makeOCIFunction("fn", "iad.ocir.io/team/fn:v1",
			map[string]string{OTelDistroConfigKey: "opentelemetry-python"}),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.True(t, snaps[0].HasOTelDistro,
		"OTEL_DISTRO=opentelemetry-python should flip HasOTelDistro")
	assert.False(t, snaps[0].HasTraceAxis,
		"no OCI_APM_ENABLED -> HasTraceAxis=false")
}

// --- Test: empty OTEL_DISTRO leaves HasOTelDistro false --------------

func TestOCIFunctionsScanner_FunctionWithEmptyOTelDistro_NoOTelDistro(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		makeOCIApplication("app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...app"] = []ociFunction{
		makeOCIFunction("fn", "iad.ocir.io/team/fn:v1",
			map[string]string{OTelDistroConfigKey: ""}),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasOTelDistro,
		"empty OTEL_DISTRO value should NOT flip HasOTelDistro")
}

// --- Test: both axes on simultaneously -------------------------------

func TestOCIFunctionsScanner_FunctionWithBoth_BothAxesTrue(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		makeOCIApplication("app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...app"] = []ociFunction{
		makeOCIFunction("fn", "iad.ocir.io/team/fn:v1",
			map[string]string{
				OCIAPMEnabledConfigKey: "true",
				OTelDistroConfigKey:    "opentelemetry-nodejs",
			}),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.True(t, snaps[0].HasTraceAxis)
	assert.True(t, snaps[0].HasOTelDistro)
	assert.True(t, snaps[0].IsInstrumented())
}

// --- Test: neither axis configured leaves both false -----------------

func TestOCIFunctionsScanner_FunctionWithNeither_BothFalse(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		makeOCIApplication("app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...app"] = []ociFunction{
		makeOCIFunction("fn", "iad.ocir.io/team/fn:v1", nil),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasTraceAxis)
	assert.False(t, snaps[0].HasOTelDistro)
	assert.False(t, snaps[0].IsInstrumented())
}

// --- Test: parent Application name surfaces in Detail map ------------

func TestOCIFunctionsScanner_ApplicationNameInDetail(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		makeOCIApplication("payments-svc"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...payments-svc"] = []ociFunction{
		makeOCIFunction("authorize", "iad.ocir.io/payments/authorize:v3", nil),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, "payments-svc", snaps[0].Detail["application"],
		`Detail["application"] should carry the parent Application name`)
	assert.Equal(t, "ACTIVE", snaps[0].Detail["lifecycle_state"])
}

// --- Test: multi-application × multi-function walk ---------------------

func TestOCIFunctionsScanner_MultiAppMultiFunction_WalksAll(t *testing.T) {
	fake := newFakeOCIFunctions()
	// Two compartments: tenancy root + one child.
	fake.Compartments = []ociCompartment{
		{ID: "ocid1.compartment.oc1..teamA", Name: "team-a", LifecycleState: "ACTIVE"},
	}
	// Root compartment has two Applications.
	fake.ApplicationsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociApplication{
		{ID: "ocid1.fnapp.oc1...root-app-1", DisplayName: "root-app-1", LifecycleState: "ACTIVE"},
		{ID: "ocid1.fnapp.oc1...root-app-2", DisplayName: "root-app-2", LifecycleState: "ACTIVE"},
	}
	// Child compartment has one Application.
	fake.ApplicationsByCompartment["ocid1.compartment.oc1..teamA"] = []ociApplication{
		{ID: "ocid1.fnapp.oc1...team-app", DisplayName: "team-app", LifecycleState: "ACTIVE"},
	}
	// root-app-1 has 2 functions; root-app-2 has 1; team-app has 1.
	fake.FunctionsByApplication["ocid1.fnapp.oc1...root-app-1"] = []ociFunction{
		makeOCIFunction("fn-1a", "iad.ocir.io/team/fn:v1", nil),
		makeOCIFunction("fn-1b", "iad.ocir.io/team/fn:v1", nil),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...root-app-2"] = []ociFunction{
		makeOCIFunction("fn-2a", "iad.ocir.io/team/fn:v1", nil),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...team-app"] = []ociFunction{
		makeOCIFunction("fn-team-a", "iad.ocir.io/team/fn:v1",
			map[string]string{OCIAPMEnabledConfigKey: "true"}),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)

	// 4 functions across 3 Applications across 2 compartments.
	require.Len(t, snaps, 4)

	// 1 compartments call (Identity list) + 2 applications calls
	// (root + child) + 3 functions calls (one per Application).
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 2, fake.ApplicationsCalls)
	assert.Equal(t, 3, fake.FunctionsCalls)

	// Exactly the team-a function has APM enabled; the other
	// three are uninstrumented.
	instrumented := 0
	for _, snap := range snaps {
		if snap.IsInstrumented() {
			instrumented++
		}
	}
	assert.Equal(t, 1, instrumented,
		"only fn-team-a (APM enabled) should be instrumented")

	// Detail map carries the parent Application name on every row.
	byName := map[string]scanner.ServerlessInstanceSnapshot{}
	for _, snap := range snaps {
		byName[snap.ResourceName] = snap
	}
	assert.Equal(t, "root-app-1", byName["fn-1a"].Detail["application"])
	assert.Equal(t, "root-app-1", byName["fn-1b"].Detail["application"])
	assert.Equal(t, "root-app-2", byName["fn-2a"].Detail["application"])
	assert.Equal(t, "team-app", byName["fn-team-a"].Detail["application"])
}

// --- Test: pagination on /applications via opc-next-page -------------

func TestOCIFunctionsScanner_PaginationFollowsOpcNextPage(t *testing.T) {
	fake := newFakeOCIFunctions()
	fake.PaginationCompartment = "ocid1.tenancy.oc1..aaa"

	// Three pages of applications: empty-token -> page1 then "p2",
	// "p2" -> page2 then "p3", "p3" -> page3 with no next token.
	app1 := ociApplication{ID: "ocid1.fnapp.oc1...a1", DisplayName: "a1", LifecycleState: "ACTIVE"}
	app2 := ociApplication{ID: "ocid1.fnapp.oc1...a2", DisplayName: "a2", LifecycleState: "ACTIVE"}
	app3 := ociApplication{ID: "ocid1.fnapp.oc1...a3", DisplayName: "a3", LifecycleState: "ACTIVE"}

	fake.AppsByPage[""] = []ociApplication{app1}
	fake.AppsByPage["p2"] = []ociApplication{app2}
	fake.AppsByPage["p3"] = []ociApplication{app3}
	fake.NextPageByPage[""] = "p2"
	fake.NextPageByPage["p2"] = "p3"
	// No NextPageByPage["p3"] entry -> the final page has no next
	// token -> loop terminates.

	// Each Application has a single function.
	fake.FunctionsByApplication["ocid1.fnapp.oc1...a1"] = []ociFunction{
		makeOCIFunction("fn-a1", "img:1", nil),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...a2"] = []ociFunction{
		makeOCIFunction("fn-a2", "img:1", nil),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...a3"] = []ociFunction{
		makeOCIFunction("fn-a3", "img:1", nil),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "three pages of one application each -> three functions")

	// Three applications calls (one per page) + three functions
	// calls (one per Application).
	assert.Equal(t, 3, fake.ApplicationsCalls,
		"opc-next-page pagination should drive three /applications calls")
	assert.Equal(t, 3, fake.FunctionsCalls)

	got := map[string]bool{}
	for _, snap := range snaps {
		got[snap.ResourceName] = true
	}
	assert.True(t, got["fn-a1"])
	assert.True(t, got["fn-a2"])
	assert.True(t, got["fn-a3"])
}

// --- Test: empty compartment surface returns empty slice -------------

func TestOCIFunctionsScanner_EmptyCompartmentReturnsEmptySlice(t *testing.T) {
	fake := newFakeOCIFunctions()
	// No applications for any compartment.

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanServerless(context.Background(), scopeRoot())
	require.NoError(t, err)
	assert.Empty(t, snaps, "compartments with no Applications return no snapshots")

	// 1 compartments call + 1 applications call against the
	// tenancy root.
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 1, fake.ApplicationsCalls)
	// Zero /functions calls — there were no Applications to walk
	// down into.
	assert.Equal(t, 0, fake.FunctionsCalls)
}
