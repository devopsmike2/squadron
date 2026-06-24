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

// --- Helpers: OCI Resource Manager + Logging httptest mock -----------
//
// fakeOCIResourceManager is a slim httptest-backed mock for slice 2
// chunk 1 OCI Resource Manager scanner tests. Routes /compartments +
// /stacks + /logs and mirrors fakeOCIStreaming's scaffolding shape.

type fakeOCIResourceManager struct {
	mu sync.Mutex

	// Compartments returned by the identity list call.
	Compartments []ociCompartment

	// StacksByCompartment maps compartmentId -> Stacks served when
	// /stacks is called with that compartmentId.
	StacksByCompartment map[string][]resourceManagerStack

	// LogsByCompartment maps compartmentId -> logs served when /logs
	// is called with that compartmentId. The scanner-side detection
	// filters to entries with Configuration.Source.Service ==
	// "resourcemanager"; the mock returns whatever is configured.
	LogsByCompartment map[string][]resourceManagerLogResource

	// PaginationCompartment, when set, opts the named compartment
	// into a multi-page /stacks response served via StacksByPage +
	// NextPageByPageStack. Empty -> single-page mode.
	PaginationCompartment string
	StacksByPage          map[string][]resourceManagerStack
	NextPageByPageStack   map[string]string

	// Call counters.
	CompartmentsCalls int
	StacksCalls       int
	LogsCalls         int

	// Per-endpoint failure switches. Status==0 means "no failure"; a
	// non-zero status returns that code with a JSON error body.
	StacksStatus       int
	LogsStatus         int
	CompartmentsStatus int

	// LogsFailureForCompartment lets a specific compartment OCID
	// trigger a per-compartment Logging call failure.
	LogsFailureForCompartment string
}

func newFakeOCIResourceManager() *fakeOCIResourceManager {
	return &fakeOCIResourceManager{
		StacksByCompartment: map[string][]resourceManagerStack{},
		LogsByCompartment:   map[string][]resourceManagerLogResource{},
		StacksByPage:        map[string][]resourceManagerStack{},
		NextPageByPageStack: map[string]string{},
	}
}

func (f *fakeOCIResourceManager) handler() http.Handler {
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

		case strings.HasSuffix(r.URL.Path, "/stacks"):
			f.StacksCalls++
			compartmentID := r.URL.Query().Get("compartmentId")
			pageToken := r.URL.Query().Get("page")

			if f.StacksStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.StacksStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}

			// Pagination opt-in path.
			if compartmentID == f.PaginationCompartment && f.PaginationCompartment != "" {
				stacks := f.StacksByPage[pageToken]
				if stacks == nil {
					stacks = []resourceManagerStack{}
				}
				if next := f.NextPageByPageStack[pageToken]; next != "" {
					w.Header().Set("opc-next-page", next)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(stacks)
				return
			}

			stacks := f.StacksByCompartment[compartmentID]
			if stacks == nil {
				stacks = []resourceManagerStack{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(stacks)
			return

		case strings.HasSuffix(r.URL.Path, "/logs"):
			f.LogsCalls++
			compartmentID := r.URL.Query().Get("compartmentId")

			if f.LogsFailureForCompartment != "" && compartmentID == f.LogsFailureForCompartment {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "Throttled", Message: "throttle"})
				return
			}

			if f.LogsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.LogsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}

			logs := f.LogsByCompartment[compartmentID]
			if logs == nil {
				logs = []resourceManagerLogResource{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(logs)
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

// newResourceManagerScannerWithFake wires a Scanner against the
// supplied fake's httptest server. Test takes ownership of cleanup
// via t.Cleanup. Mirrors newStreamingScannerWithFake but targets the
// chunk 1 resource manager mock specifically.
func newResourceManagerScannerWithFake(t *testing.T, fake *fakeOCIResourceManager, region string) *Scanner {
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

func makeOCIStack(name, compartmentID string) resourceManagerStack {
	return resourceManagerStack{
		ID:             "ocid1.ormstack.oc1..." + name,
		DisplayName:    name,
		CompartmentID:  compartmentID,
		LifecycleState: "ACTIVE",
		TimeCreated:    "2026-01-01T00:00:00Z",
	}
}

func makeRMLogResource(compartmentID, service string) resourceManagerLogResource {
	return resourceManagerLogResource{
		ID:          "ocid1.log.oc1..." + service,
		LogGroupID:  "ocid1.loggroup.oc1..." + service,
		DisplayName: "log-for-" + service,
		Configuration: resourceManagerLogConfiguration{
			Source: resourceManagerLogSourceWire{
				Service:    service,
				SourceType: "OCISERVICE",
			},
		},
	}
}

// --- Test 1 (slice 2 §11): paginated list returns Stacks -----------

func TestScanResourceManagerStacks_PaginatedListReturnsStacks(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.PaginationCompartment = "ocid1.tenancy.oc1..aaa"

	// Three pages of stacks: empty-token -> page1 then "p2",
	// "p2" -> page2 then "p3", "p3" -> page3 with no next token.
	s1 := makeOCIStack("stack-1", "ocid1.tenancy.oc1..aaa")
	s2 := makeOCIStack("stack-2", "ocid1.tenancy.oc1..aaa")
	s3 := makeOCIStack("stack-3", "ocid1.tenancy.oc1..aaa")

	fake.StacksByPage[""] = []resourceManagerStack{s1}
	fake.StacksByPage["p2"] = []resourceManagerStack{s2}
	fake.StacksByPage["p3"] = []resourceManagerStack{s3}
	fake.NextPageByPageStack[""] = "p2"
	fake.NextPageByPageStack["p2"] = "p3"

	sc := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := sc.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "three pages of one stack each -> three snapshots")

	assert.Equal(t, 3, fake.StacksCalls,
		"opc-next-page pagination should drive three /stacks calls")

	got := map[string]bool{}
	for _, snap := range snaps {
		got[snap.ResourceName] = true
	}
	assert.True(t, got["stack-1"])
	assert.True(t, got["stack-2"])
	assert.True(t, got["stack-3"])
}

// --- Test 2 (slice 2 §11): Stack with Logging compartment + RM source
// mapping -> has_log_axis = true. -----------------------------------

func TestScanResourceManagerStacks_StackWithLoggingCompartmentRMSource_HasLogAxis(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("prod-stack", "ocid1.tenancy.oc1..aaa"),
	}
	fake.LogsByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerLogResource{
		makeRMLogResource("ocid1.tenancy.oc1..aaa", OCIResourceManagerLogSourceService),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasLogAxis,
		"matching Logging log resource with resourcemanager service should flip HasLogAxis")
	assert.False(t, snap.HasTraceAxis,
		"OCI does not expose direct OTel for RM in slice 2; HasTraceAxis must stay false")
	assert.Equal(t, "oci", snap.Provider)
	assert.Equal(t, OCIResourceManagerSurface, snap.Surface)
	assert.Equal(t, "prod-stack", snap.ResourceName)
}

// --- Test 3 (slice 2 §11): Stack with Logging compartment but NO RM
// source mapping -> has_log_axis = false. ---------------------------

func TestScanResourceManagerStacks_StackWithLoggingCompartmentNoRMSource_NoLogAxis(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("orphan-stack", "ocid1.tenancy.oc1..aaa"),
	}
	// Logging present in the compartment, but the source mapping
	// targets a different service (e.g. "objectstorage" instead of
	// "resourcemanager"). Strict per-§3.4 detection: no RM source ->
	// HasLogAxis stays false.
	fake.LogsByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerLogResource{
		makeRMLogResource("ocid1.tenancy.oc1..aaa", "objectstorage"),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.False(t, snap.HasLogAxis,
		"Logging present but no resourcemanager source -> HasLogAxis stays false (operator-strict per §3.4)")
}

// --- Test 4 (slice 2 §11): Stack with NO Logging at compartment
// level -> has_log_axis = false. ------------------------------------

func TestScanResourceManagerStacks_StackWithNoCompartmentLogging_NoLogAxis(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("untracked-stack", "ocid1.tenancy.oc1..aaa"),
	}
	// LogsByCompartment intentionally empty -> the /logs call
	// returns an empty list.

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.False(t, snap.HasLogAxis,
		"no Logging entry in compartment should leave HasLogAxis false")
	assert.False(t, snap.HasTraceAxis,
		"HasTraceAxis is always false in slice 2 chunk 1")
	assert.False(t, snap.IsInstrumented(),
		"a Stack with neither axis is uninstrumented")
}

// --- Test: multiple compartments walk all ---------------------------

func TestScanResourceManagerStacks_MultipleCompartments_WalksAll(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.Compartments = []ociCompartment{
		{ID: "ocid1.compartment.oc1..teamA", Name: "team-a", LifecycleState: "ACTIVE"},
		{ID: "ocid1.compartment.oc1..teamB", Name: "team-b", LifecycleState: "ACTIVE"},
	}
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("root-stack", "ocid1.tenancy.oc1..aaa"),
	}
	fake.StacksByCompartment["ocid1.compartment.oc1..teamA"] = []resourceManagerStack{
		makeOCIStack("team-a-stack", "ocid1.compartment.oc1..teamA"),
	}
	fake.StacksByCompartment["ocid1.compartment.oc1..teamB"] = []resourceManagerStack{
		makeOCIStack("team-b-stack-1", "ocid1.compartment.oc1..teamB"),
		makeOCIStack("team-b-stack-2", "ocid1.compartment.oc1..teamB"),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 4, "1 root + 1 team-a + 2 team-b = 4 stacks")

	// 1 compartments call (Identity list) + 3 stacks calls.
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 3, fake.StacksCalls)
	// 3 /logs calls (one per compartment, not per stack).
	assert.Equal(t, 3, fake.LogsCalls,
		"per-compartment Logging detection — one /logs call per compartment regardless of stack count")

	names := map[string]bool{}
	for _, snap := range snaps {
		names[snap.ResourceName] = true
	}
	assert.True(t, names["root-stack"])
	assert.True(t, names["team-a-stack"])
	assert.True(t, names["team-b-stack-1"])
	assert.True(t, names["team-b-stack-2"])
}

// --- Test: ResourceName + ResourceARN populated correctly -----------

func TestScanResourceManagerStacks_StackResourceNamePopulated(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		{
			ID:             "ocid1.ormstack.oc1.iad.aaaaaaaa1234567890abcdef",
			DisplayName:    "production-infra",
			CompartmentID:  "ocid1.tenancy.oc1..aaa",
			LifecycleState: "ACTIVE",
			TimeCreated:    "2026-01-01T00:00:00Z",
		},
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-ashburn-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.Equal(t, "production-infra", snap.ResourceName,
		"ResourceName should carry the stack's DisplayName")
	assert.Equal(t, "us-ashburn-1", snap.Region,
		"Region should carry the scanner's configured Region")
}

func TestScanResourceManagerStacks_StackARNPopulated(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		{
			ID:             "ocid1.ormstack.oc1.iad.aaaaaaaa1234567890abcdef",
			DisplayName:    "any-stack",
			CompartmentID:  "ocid1.tenancy.oc1..aaa",
			LifecycleState: "ACTIVE",
		},
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, "ocid1.ormstack.oc1.iad.aaaaaaaa1234567890abcdef", snaps[0].ResourceARN,
		"ResourceARN should carry the stack's full OCID")
}

// --- Test: WorkflowType is "Stack" ---------------------------------

func TestScanResourceManagerStacks_WorkflowTypeIsStack(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("any-stack", "ocid1.tenancy.oc1..aaa"),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, OCIResourceManagerWorkflowType, snaps[0].WorkflowType,
		"OCI Resource Manager snapshots must carry WorkflowType=Stack per the per-cloud convention")
}

// --- Test: Surface is "resmgr" -------------------------------------

func TestScanResourceManagerStacks_SurfaceIsResmgr(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("any-stack", "ocid1.tenancy.oc1..aaa"),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, "resmgr", snaps[0].Surface,
		"OCI Resource Manager snapshots must carry Surface=resmgr per design doc §3.4")
	assert.Equal(t, OCIResourceManagerSurface, snaps[0].Surface,
		"Surface constant should match")
	assert.Equal(t, "oci", snaps[0].Provider)
}

// --- Test: HasTraceAxis is always false in slice 2 chunk 1 ---------

func TestScanResourceManagerStacks_HasTraceAxisAlwaysFalse(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("traced-stack", "ocid1.tenancy.oc1..aaa"),
	}
	// Logging configured with RM source -> HasLogAxis flips true,
	// but HasTraceAxis must stay false per §3.4 (OCI does not
	// expose direct OTel integration for Resource Manager in
	// slice 2).
	fake.LogsByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerLogResource{
		makeRMLogResource("ocid1.tenancy.oc1..aaa", OCIResourceManagerLogSourceService),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasTraceAxis,
		"slice 2 chunk 1 does not claim trace axis for Resource Manager; HasTraceAxis must stay false")
}

// --- Test: Logging API failure is non-fatal (partial-scan posture) ---

func TestScanResourceManagerStacks_LoggingAPIFailureNonFatal(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("stack-a", "ocid1.tenancy.oc1..aaa"),
		makeOCIStack("stack-b", "ocid1.tenancy.oc1..aaa"),
	}
	// All Logging calls for this compartment fail; the stacks must
	// still surface with HasLogAxis=false.
	fake.LogsFailureForCompartment = "ocid1.tenancy.oc1..aaa"

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err, "Logging API failure must not abort the walk")
	require.Len(t, snaps, 2, "both stacks still surface even when Logging fails")

	for _, snap := range snaps {
		assert.False(t, snap.HasLogAxis,
			"Logging call failed -> HasLogAxis defaults to false (partial-scan posture)")
	}
}

// --- Test: empty compartment surface returns empty slice -----------

func TestScanResourceManagerStacks_EmptyCompartmentReturnsEmptySlice(t *testing.T) {
	fake := newFakeOCIResourceManager()
	// No stacks for any compartment.

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanResourceManagerStacks(context.Background(), scopeRoot())
	require.NoError(t, err)
	assert.Empty(t, snaps, "compartments with no stacks return no snapshots")

	// 1 compartments call + 1 stacks call against the tenancy root.
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 1, fake.StacksCalls)
}

// --- Test 5 (slice 2 §11): ScanOrchestrations dispatches to
// ScanResourceManagerStacks. ----------------------------------------

func TestScanOrchestrations_DispatchesToScanResourceManagerStacks(t *testing.T) {
	fake := newFakeOCIResourceManager()
	fake.StacksByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerStack{
		makeOCIStack("dispatched-stack", "ocid1.tenancy.oc1..aaa"),
	}
	fake.LogsByCompartment["ocid1.tenancy.oc1..aaa"] = []resourceManagerLogResource{
		makeRMLogResource("ocid1.tenancy.oc1..aaa", OCIResourceManagerLogSourceService),
	}

	s := newResourceManagerScannerWithFake(t, fake, "us-phoenix-1")
	delegated, err := s.ScanOrchestrations(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, delegated, 1,
		"ScanOrchestrations should delegate to ScanResourceManagerStacks and surface the same stack")
	assert.Equal(t, OCIResourceManagerSurface, delegated[0].Surface,
		"the dispatched snapshot should carry the resmgr surface")
	assert.Equal(t, "dispatched-stack", delegated[0].ResourceName)
	assert.True(t, delegated[0].HasLogAxis, "Logging detection should flow through the dispatcher")
	assert.False(t, delegated[0].HasTraceAxis, "HasTraceAxis stays false through the dispatcher")
}

// --- Test: missing required fields -> error (regression — match the
// existing ScanStreams / ScanFunctions posture). The OCI scanner has
// no signingClient concept; it parses PEM bytes on demand. The
// closest parity check is "missing PrivateKey / Region returns an
// error rather than crashing or returning garbage". ----------------

func TestScanOrchestrations_SigningClientNil_GracefullyReturnsEmpty(t *testing.T) {
	// PrivateKey omitted -> signing key parse failure surfaces as
	// an error (matches the existing OCI scanner posture).
	s := &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		Region:      "us-phoenix-1",
	}
	snaps, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	assert.Error(t, err, "missing PrivateKey should surface as an error")
	assert.Empty(t, snaps, "no snapshots leak on the error path")

	// Missing Region — distinct error.
	s2 := &Scanner{TenancyOCID: "ocid1.tenancy.oc1..aaa"}
	snaps2, err2 := s2.ScanOrchestrations(context.Background(), scanner.ScanScope{})
	assert.Error(t, err2, "missing Region should surface as an error")
	assert.Empty(t, snaps2)
}

// --- Test: classifyOCIResourceManagerError surface (smoke test on
// the error helper exposed for chunk 2's trampoline integration). ----

func TestScanResourceManagerStacks_ClassifyError_Surface(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		atRoot     bool
		wantPrefix string
		wantEmpty  bool
	}{
		{"401", &ociCallError{StatusCode: http.StatusUnauthorized}, false, ServiceIDOrchestration + ": credentials invalid", false},
		{"403", &ociCallError{StatusCode: http.StatusForbidden, Message: "policy"}, false, ServiceIDOrchestration + ": permission denied", false},
		{"404_midwalk", &ociCallError{StatusCode: http.StatusNotFound}, false, "", true},
		{"404_root", &ociCallError{StatusCode: http.StatusNotFound}, true, ServiceIDOrchestration + ": Resource Manager surface not found", false},
		{"429", &ociCallError{StatusCode: http.StatusTooManyRequests}, false, ServiceIDOrchestration + ": rate limit exceeded", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyOCIResourceManagerError(tc.err, tc.atRoot)
			if tc.wantEmpty {
				assert.Equal(t, "", got)
				return
			}
			assert.True(t, strings.HasPrefix(got, tc.wantPrefix), "got %q want prefix %q", got, tc.wantPrefix)
		})
	}
}
