// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Orchestration tier slice 1 chunk 2 (v0.89.96, #729 Stream 127) GCP
// Workflows scanner tests. The test cases pin docs/proposals/
// orchestration-tier-slice1.md §11 acceptance tests 5 + 6 (the GCP
// Workflows rows of the per-cloud detection matrix), plus the
// pagination + empty-response + ARN-parse posture tests that mirror
// the AWS Step Functions scanner's slice 1 chunk 1 test surface.
//
// The tests run a self-contained httptest fake that serves the
// workflows.googleapis.com endpoints the scanner exercises:
//
//   - GET /v1/projects/{project}/locations          (Locations list)
//   - GET /v1/projects/{project}/locations/{r}/workflows  (Workflows list)
//
// Keeping the fake local (rather than extending the existing fakeGCP
// in scanner_test.go) avoids cross-cutting churn into the slice-1
// scanner test surface and matches the chunk 2 worktree boundary —
// the orchestration walk is dispatched separately via
// ScanOrchestrations and does not feed into Scan().

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
	workflows "google.golang.org/api/workflows/v1"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// --- Test doubles -----------------------------------------------------

// fakeWorkflows is an httptest-backed mock of the GCP Workflows REST
// API. It implements the two endpoints the scanner walks
// (Projects.Locations.List and Projects.Locations.Workflows.List),
// which is enough to exercise every code path in workflows.go without
// standing up real GCP credentials.
type fakeWorkflows struct {
	mu sync.Mutex

	// WorkflowsByRegion maps region -> single-page workflow list. The
	// fake returns an empty slice when the region isn't seeded.
	WorkflowsByRegion map[string][]*workflows.Workflow

	// PagesByRegion, when set for a region, makes the workflows.list
	// endpoint page through the supplied page sequence. Each page
	// returns its workflows and a nextPageToken pointing at the next
	// page (or empty for the last page). The fake pulls the page by
	// index based on the inbound pageToken query param.
	PagesByRegion map[string][][]*workflows.Workflow

	// Locations is the static locations list served by the Locations
	// endpoint when the scanner needs to enumerate regions.
	Locations []*workflows.Location

	// WorkflowsListStatus, when non-zero, makes the next workflows
	// list call return this status (with a googleapi-style body).
	WorkflowsListStatus int

	// LocationsListStatus, when non-zero, makes the next locations
	// list call return this status.
	LocationsListStatus int

	// Call counters for assertions.
	WorkflowsListCalls map[string]int
	LocationsListCalls int
}

func newFakeWorkflows() *fakeWorkflows {
	return &fakeWorkflows{
		WorkflowsByRegion:  map[string][]*workflows.Workflow{},
		PagesByRegion:      map[string][][]*workflows.Workflow{},
		WorkflowsListCalls: map[string]int{},
	}
}

func (f *fakeWorkflows) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		// Workflows API path shapes (REST, v1):
		//   /v1/projects/{project}/locations
		//   /v1/projects/{project}/locations/{region}/workflows
		switch {
		case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/workflows"):
			region := parseRegionFromWorkflowsPath(path)
			f.WorkflowsListCalls[region]++
			if f.WorkflowsListStatus != 0 {
				writeAPIError(w, f.WorkflowsListStatus,
					statusReason(f.WorkflowsListStatus),
					statusName(f.WorkflowsListStatus))
				return
			}
			items, nextToken := f.workflowsPage(region, r.URL.Query().Get("pageToken"))
			resp := workflows.ListWorkflowsResponse{
				Workflows:     items,
				NextPageToken: nextToken,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return

		case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/locations"):
			f.LocationsListCalls++
			if f.LocationsListStatus != 0 {
				writeAPIError(w, f.LocationsListStatus,
					statusReason(f.LocationsListStatus),
					statusName(f.LocationsListStatus))
				return
			}
			resp := workflows.ListLocationsResponse{Locations: f.Locations}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Unmatched path — surface as 404 so test failures are obvious.
		writeAPIError(w, http.StatusNotFound,
			fmt.Sprintf("unhandled mock path: %s", path), "NOT_FOUND")
	})
}

// parseRegionFromWorkflowsPath extracts the region segment from a
// workflows-list URL of shape ".../locations/{region}/workflows".
func parseRegionFromWorkflowsPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// workflowsPage resolves the inbound pageToken into the (items,
// nextToken) pair the fake should return for the given region. The
// token shape is "page-N" (1-indexed); the empty token selects page 0
// (or the flat WorkflowsByRegion list when no pages are configured).
func (f *fakeWorkflows) workflowsPage(region, pageToken string) ([]*workflows.Workflow, string) {
	pages, ok := f.PagesByRegion[region]
	if !ok || len(pages) == 0 {
		return f.WorkflowsByRegion[region], ""
	}
	idx := 0
	if pageToken != "" {
		var n int
		_, _ = fmt.Sscanf(pageToken, "page-%d", &n)
		idx = n
	}
	if idx >= len(pages) {
		return nil, ""
	}
	items := pages[idx]
	if idx+1 < len(pages) {
		return items, fmt.Sprintf("page-%d", idx+1)
	}
	return items, ""
}

// newScannerWithWorkflowsFake wires a Scanner against the supplied
// fake's httptest server. Mirrors newScannerWithFake (scanner_test.go)
// but uses a standalone fake to keep the orchestration walk's surface
// disjoint from the other walks.
func newScannerWithWorkflowsFake(t *testing.T, fake *fakeWorkflows, projectID, region string) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		ProjectID:  projectID,
		SAJSON:     nil,
		Region:     region,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
}

// makeWorkflow — fixture builder. Wires the two detection axes (the
// callLogLevel) through to the workflows.Workflow shape so each test
// can construct exactly the minimal workflow it needs to exercise one
// axis. The fixture sets Name to the canonical
// "projects/{p}/locations/{r}/workflows/{n}" shape so the projection's
// ResourceName / ResourceARN read honestly.
func makeWorkflow(project, region, name, callLogLevel string) *workflows.Workflow {
	return &workflows.Workflow{
		Name: fmt.Sprintf("projects/%s/locations/%s/workflows/%s",
			project, region, name),
		CallLogLevel: callLogLevel,
		State:        "ACTIVE",
	}
}

// runWorkflowsScan is the shared harness for the per-axis tests. It
// wires a fake into a fresh Scanner pinned to the supplied region and
// calls ScanWorkflows against that region (so the scope.Regions
// branch is exercised — the Locations enumeration is covered by a
// dedicated test). Returns the snapshots so the caller can assert
// per-axis outcomes without re-implementing the wiring boilerplate
// per test.
func runWorkflowsScan(t *testing.T, fake *fakeWorkflows, project, region string) []scanner.OrchestrationInstanceSnapshot {
	t.Helper()
	s := newScannerWithWorkflowsFake(t, fake, project, region)
	out, err := s.ScanWorkflows(context.Background(), scanner.ScanScope{
		Regions:   []string{region},
		AccountID: project,
	})
	require.NoError(t, err)
	return out
}

// --- Acceptance tests -------------------------------------------------

// TestWorkflowsScanner_WorkflowWithLogAllCalls_HasTraceAxis — Slice 1
// acceptance test 5: a workflow with callLogLevel=LOG_ALL_CALLS flips
// both the HasTraceAxis (soft proxy) and HasLogAxis axes. Pins the
// design doc §3.2 detection rule.
func TestWorkflowsScanner_WorkflowWithLogAllCalls_HasTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "checkout"
	)
	fake := newFakeWorkflows()
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{
		makeWorkflow(project, region, name, WorkflowsCallLogLevelLogAll),
	}
	out := runWorkflowsScan(t, fake, project, region)
	require.Len(t, out, 1)
	snap := out[0]
	assert.Equal(t, string(credstore.ProviderGCP), snap.Provider)
	assert.Equal(t, "workflows", snap.Surface)
	assert.Equal(t, project, snap.AccountID)
	assert.Equal(t, region, snap.Region)
	assert.Equal(t, name, snap.ResourceName)
	assert.Equal(t,
		fmt.Sprintf("projects/%s/locations/%s/workflows/%s", project, region, name),
		snap.ResourceARN)
	assert.True(t, snap.HasTraceAxis, "LOG_ALL_CALLS must flip the trace axis (soft proxy)")
	assert.True(t, snap.HasLogAxis, "LOG_ALL_CALLS must also flip the log axis")
	assert.Equal(t, WorkflowsCallLogLevelLogAll, snap.Detail["call_log_level"])
	// Slice 1 instrumented rule: either axis presence flips the
	// snapshot.IsInstrumented() predicate.
	assert.True(t, snap.IsInstrumented())
}

// TestWorkflowsScanner_WorkflowWithCallLogLevelUnspecified_NoTraceAxisOrLogAxis —
// Slice 1 acceptance test 6: a workflow with
// callLogLevel=CALL_LOG_LEVEL_UNSPECIFIED leaves both axes false.
// Pins the negative case of the design doc §3.2 detection rule.
func TestWorkflowsScanner_WorkflowWithCallLogLevelUnspecified_NoTraceAxisOrLogAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "orders"
	)
	fake := newFakeWorkflows()
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{
		makeWorkflow(project, region, name, WorkflowsCallLogLevelUnspecified),
	}
	out := runWorkflowsScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"trace axis must stay false when callLogLevel is CALL_LOG_LEVEL_UNSPECIFIED")
	assert.False(t, out[0].HasLogAxis,
		"log axis must stay false when callLogLevel is CALL_LOG_LEVEL_UNSPECIFIED")
	assert.False(t, out[0].IsInstrumented())
}

// TestWorkflowsScanner_WorkflowWithLogErrorsOnly_HasLogAxisOnly — a
// workflow with callLogLevel=LOG_ERRORS_ONLY flips the log axis but
// not the trace axis. The trace axis is the LOG_ALL_CALLS-specific
// soft proxy; ERROR-only logging is honest log presence but does NOT
// imply trace emission.
func TestWorkflowsScanner_WorkflowWithLogErrorsOnly_HasLogAxisOnly(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "audit"
	)
	fake := newFakeWorkflows()
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{
		makeWorkflow(project, region, name, "LOG_ERRORS_ONLY"),
	}
	out := runWorkflowsScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"trace axis is only flipped by LOG_ALL_CALLS — LOG_ERRORS_ONLY does not qualify")
	assert.True(t, out[0].HasLogAxis,
		"LOG_ERRORS_ONLY flips the log axis (any non-UNSPECIFIED / non-NONE level qualifies)")
	assert.True(t, out[0].IsInstrumented())
}

// TestWorkflowsScanner_WorkflowWithLogNone_HasLogAxisFalse — the
// LOG_NONE carve-out documented on WorkflowsCallLogLevelUnspecified.
// LOG_NONE is a valid GCP-published level value but represents an
// explicit "logging disabled" operator intent; Squadron's
// recommendation surface treats it as HasLogAxis=false so the
// proposer surfaces a workflows-logging-enable recommendation rather
// than silently counting it as wired.
func TestWorkflowsScanner_WorkflowWithLogNone_HasLogAxisFalse(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "silent"
	)
	fake := newFakeWorkflows()
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{
		makeWorkflow(project, region, name, WorkflowsCallLogLevelNone),
	}
	out := runWorkflowsScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"trace axis stays false on LOG_NONE")
	assert.False(t, out[0].HasLogAxis,
		"LOG_NONE represents 'logging disabled' — must NOT flip the log axis despite being non-UNSPECIFIED")
	assert.False(t, out[0].IsInstrumented())
	assert.Equal(t, WorkflowsCallLogLevelNone, out[0].Detail["call_log_level"],
		"raw level still surfaces in the Detail bag for the Inventory drilldown")
}

// TestWorkflowsScanner_PaginationFollowsNextPageToken — two list
// pages surface three total workflows. The scanner must follow
// nextPageToken until the response carries an empty token. Mirrors
// the AWS Step Functions scanner's
// TestStepFunctionsScanner_PaginationFollowsNextToken contract.
func TestWorkflowsScanner_PaginationFollowsNextPageToken(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeWorkflows()
	fake.PagesByRegion[region] = [][]*workflows.Workflow{
		{
			makeWorkflow(project, region, "a", WorkflowsCallLogLevelLogAll),
			makeWorkflow(project, region, "b", WorkflowsCallLogLevelUnspecified),
		},
		{
			makeWorkflow(project, region, "c", "LOG_ERRORS_ONLY"),
		},
	}
	out := runWorkflowsScan(t, fake, project, region)
	require.Len(t, out, 3, "both list pages must surface — three workflows total")
	names := map[string]bool{}
	for _, snap := range out {
		names[snap.ResourceName] = true
	}
	assert.True(t, names["a"])
	assert.True(t, names["b"])
	assert.True(t, names["c"])
}

// TestWorkflowsScanner_EmptyResponseReturnsEmptySlice — zero
// workflows in the region surface as an empty result without error.
func TestWorkflowsScanner_EmptyResponseReturnsEmptySlice(t *testing.T) {
	const region = "us-central1"
	fake := newFakeWorkflows()
	// Region keyed but seeded empty — the fake returns an empty slice
	// rather than a 404.
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{}
	out := runWorkflowsScan(t, fake, "test-project", region)
	assert.Len(t, out, 0)
}

// TestWorkflowsScanner_ResourceNameAndARNPopulated — the projection
// reads ResourceARN from wf.Name verbatim and trims ResourceName from
// the trailing path segment. The slice 1 chunk 2 contract: the
// proposer's evidence list and the recommendation envelope's
// AffectedResources field both reference ResourceARN; the Inventory
// tab renders ResourceName.
func TestWorkflowsScanner_ResourceNameAndARNPopulated(t *testing.T) {
	const (
		project = "test-project"
		region  = "europe-west1"
		name    = "payments-pipeline"
	)
	fake := newFakeWorkflows()
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{
		makeWorkflow(project, region, name, WorkflowsCallLogLevelLogAll),
	}
	out := runWorkflowsScan(t, fake, project, region)
	require.Len(t, out, 1)
	snap := out[0]
	assert.Equal(t, name, snap.ResourceName, "ResourceName trims to the trailing path segment")
	assert.Equal(t,
		fmt.Sprintf("projects/%s/locations/%s/workflows/%s", project, region, name),
		snap.ResourceARN,
		"ResourceARN reads wf.Name verbatim")
	assert.Equal(t, region, snap.Region,
		"Region is the denormalized projection from the walker's region argument")
}

// TestWorkflowsScanner_LocationsEnumeratedWhenRegionEmpty — when
// neither scope.Regions nor s.Region is set, the scanner enumerates
// locations via Projects.Locations.List and walks each. Pins the
// no-region-pinned operator config posture.
func TestWorkflowsScanner_LocationsEnumeratedWhenRegionEmpty(t *testing.T) {
	const project = "test-project"
	fake := newFakeWorkflows()
	fake.Locations = []*workflows.Location{
		{LocationId: "us-central1"},
		{LocationId: "europe-west1"},
	}
	fake.WorkflowsByRegion["us-central1"] = []*workflows.Workflow{
		makeWorkflow(project, "us-central1", "alpha", WorkflowsCallLogLevelLogAll),
	}
	fake.WorkflowsByRegion["europe-west1"] = []*workflows.Workflow{
		makeWorkflow(project, "europe-west1", "beta", "LOG_ERRORS_ONLY"),
	}

	s := newScannerWithWorkflowsFake(t, fake, project, "")
	out, err := s.ScanWorkflows(context.Background(), scanner.ScanScope{
		AccountID: project,
	})
	require.NoError(t, err)
	require.Len(t, out, 2, "both regions enumerated via Locations.List must surface")
	// Confirm the Locations endpoint was hit; the Workflows endpoint
	// was called once per enumerated region.
	assert.Equal(t, 1, fake.LocationsListCalls)
	assert.Equal(t, 1, fake.WorkflowsListCalls["us-central1"])
	assert.Equal(t, 1, fake.WorkflowsListCalls["europe-west1"])
}

// TestWorkflowsScanner_ScanOrchestrationsDelegatesToScanWorkflows —
// the chunk 1 OrchestrationDiscoveryScanner interface dispatch.
// Asserts that the orchestration entry point produces the same
// snapshots as the direct Workflows walk so the handler-side runtime
// type assertion lands on the correct method.
func TestWorkflowsScanner_ScanOrchestrationsDelegatesToScanWorkflows(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "demo"
	)
	fake := newFakeWorkflows()
	fake.WorkflowsByRegion[region] = []*workflows.Workflow{
		makeWorkflow(project, region, name, WorkflowsCallLogLevelLogAll),
	}
	s := newScannerWithWorkflowsFake(t, fake, project, region)
	out, err := s.ScanOrchestrations(context.Background(), scanner.ScanScope{
		Regions:   []string{region},
		AccountID: project,
	})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "workflows", out[0].Surface)
	assert.Equal(t, string(credstore.ProviderGCP), out[0].Provider)
	assert.True(t, out[0].HasTraceAxis)
}

// TestWorkflowsScanner_PerRegionFailureIsNonFatal — a failing region
// must not abort the whole scan. The scanner walks the remaining
// regions and returns whatever snapshots succeeded. Mirrors the AWS
// scanner's per-machine-describe-failure posture.
func TestWorkflowsScanner_PerRegionFailureIsNonFatal(t *testing.T) {
	const project = "test-project"
	fake := newFakeWorkflows()
	// us-central1 returns a valid response; the next List call will
	// fail (the fake flips its status flag globally — we exercise the
	// loop's continue branch by configuring a single region success
	// followed by a failure injection on the next iteration). Since
	// the fake's status flag is global, we instead simulate by
	// configuring a single failing region and asserting no error
	// surfaces from ScanWorkflows.
	fake.WorkflowsListStatus = http.StatusForbidden

	s := newScannerWithWorkflowsFake(t, fake, project, "us-central1")
	out, err := s.ScanWorkflows(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-central1"},
		AccountID: project,
	})
	require.NoError(t, err,
		"per-region list failures must NOT propagate as scan error — the chunk-2 partial-scan posture")
	assert.Len(t, out, 0,
		"the failing region's empty result is the only data; total is zero")
}
