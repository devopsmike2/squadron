// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

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
	compute "google.golang.org/api/compute/v1"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// --- Test doubles -----------------------------------------------------
//
// fakeGCP is an httptest-backed mock of the GCP compute REST API. It
// only implements the two endpoints slice 1 walks (zones.list and
// instances.list), which is enough to exercise every code path in
// scanner.go without standing up real GCP credentials.
//
// Tests seed Zones and InstancesByZone with the response shape they
// want; the mock dispatches based on URL path. Failure tests set the
// matching *ErrStatus field so the next call to that endpoint returns
// the configured status code instead of a successful response. The
// ErrStatus fields are checked once per zone (typical use); tests
// that need per-zone control use ZoneOverrides.

type fakeGCP struct {
	mu sync.Mutex

	// Zones is the static zones list served by zones.list.
	Zones []*compute.Zone

	// InstancesByZone maps zone name → instances list response. The
	// mock returns an empty list when a zone isn't seeded.
	InstancesByZone map[string][]*compute.Instance

	// ZonesListStatus, when non-zero, makes the next zones.list call
	// return this status (with a googleapi-style body).
	ZonesListStatus int

	// InstancesListStatusByZone, when set for a zone, makes the
	// instances.list call for that zone return the configured status.
	InstancesListStatusByZone map[string]int

	// Call counters for assertions.
	ZonesListCalls     int
	InstancesListCalls map[string]int
}

func newFakeGCP() *fakeGCP {
	return &fakeGCP{
		InstancesByZone:           map[string][]*compute.Instance{},
		InstancesListStatusByZone: map[string]int{},
		InstancesListCalls:        map[string]int{},
	}
}

// errorResponseBody is the JSON shape googleapi.CheckResponse parses
// to populate googleapi.Error.Code + .Message. Mirrors the on-wire
// shape the real GCP API returns so the scanner's classifyZonesListError
// path sees the same struct it'd see in production.
type errorResponseBody struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func writeAPIError(w http.ResponseWriter, code int, message, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorResponseBody{
		Error: errorBody{
			Code:    code,
			Message: message,
			Status:  status,
		},
	})
}

// handler is the http.HandlerFunc the mock serves. It pattern-matches
// against the URL path to dispatch zones.list vs instances.list.
func (f *fakeGCP) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		// Compute API path shapes (REST, v1):
		//   /compute/v1/projects/{project}/zones
		//   /compute/v1/projects/{project}/zones/{zone}/instances
		switch {
		case strings.HasSuffix(path, "/zones"):
			f.ZonesListCalls++
			if f.ZonesListStatus != 0 {
				writeAPIError(w, f.ZonesListStatus, statusReason(f.ZonesListStatus), statusName(f.ZonesListStatus))
				return
			}
			resp := compute.ZoneList{Items: f.Zones}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return

		case strings.Contains(path, "/zones/") && strings.HasSuffix(path, "/instances"):
			zone := parseZoneFromInstancesPath(path)
			f.InstancesListCalls[zone]++
			if code, ok := f.InstancesListStatusByZone[zone]; ok && code != 0 {
				writeAPIError(w, code, statusReason(code), statusName(code))
				return
			}
			resp := compute.InstanceList{Items: f.InstancesByZone[zone]}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Unmatched path — surface as 404 so test failures are
		// obvious (rather than the scanner silently consuming an
		// empty body).
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path), "NOT_FOUND")
	})
}

func parseZoneFromInstancesPath(path string) string {
	// Path shape: ".../zones/{zone}/instances" (with optional trailing
	// query). The mock pre-strips the query in net/http.URL.Path.
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "zones" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func statusReason(code int) string {
	switch code {
	case http.StatusForbidden:
		return "Request had insufficient authentication scopes."
	case http.StatusNotFound:
		return "Requested entity was not found."
	case http.StatusTooManyRequests:
		return "Quota exceeded."
	default:
		return http.StatusText(code)
	}
}

func statusName(code int) string {
	switch code {
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	default:
		return http.StatusText(code)
	}
}

// newScannerWithFake wires a Scanner against the supplied fake's
// httptest server. The test takes ownership of cleanup via t.Cleanup.
func newScannerWithFake(t *testing.T, fake *fakeGCP, projectID, region string) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		ProjectID:  projectID,
		SAJSON:     nil, // bypassed via httpClient+endpoint
		Region:     region,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
}

// --- Tests ------------------------------------------------------------

func TestScan_ReturnsInstancesWithComputeInstanceSnapshotShape(t *testing.T) {
	fake := newFakeGCP()
	fake.Zones = []*compute.Zone{
		{Name: "us-central1-a"},
		{Name: "us-west1-a"},
	}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{
			Name:        "web-1",
			MachineType: "zones/us-central1-a/machineTypes/n2-standard-4",
			Labels:      map[string]string{"env": "prod"},
		},
		{
			Name:        "web-2",
			MachineType: "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/machineTypes/e2-medium",
			Labels:      map[string]string{"otel-collector": "v1"},
		},
	}
	fake.InstancesByZone["us-west1-a"] = []*compute.Instance{
		{
			Name:        "worker-1",
			MachineType: "zones/us-west1-a/machineTypes/n1-standard-2",
		},
	}

	s := newScannerWithFake(t, fake, "test-project", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 3, "expected 3 snapshot entries across both zones")
	assert.Equal(t, credstore.ProviderGCP, res.Provider)
	assert.Equal(t, "test-project", res.AccountID)
	assert.False(t, res.Partial)
	assert.Empty(t, res.PartialReason)
	assert.Empty(t, res.FailedServices)
	assert.NotEmpty(t, res.ScanID)
	assert.False(t, res.ScanStartedAt.IsZero())
	assert.False(t, res.ScanCompletedAt.IsZero())

	// Build a map for stable assertion lookup (Compute order tracks
	// zone iteration order, which is API-stable but the test reads
	// nicer when keyed by ResourceID).
	byID := map[string]int{}
	for i, c := range res.Compute {
		byID[c.ResourceID] = i
	}
	require.Contains(t, byID, "web-1")
	require.Contains(t, byID, "web-2")
	require.Contains(t, byID, "worker-1")

	web1 := res.Compute[byID["web-1"]]
	assert.Equal(t, "n2-standard-4", web1.InstanceType)
	assert.Equal(t, "us-central1", web1.Region)
	assert.Equal(t, "unknown", web1.OSFamily)
	assert.Equal(t, map[string]string{"env": "prod"}, web1.Tags)
	assert.False(t, web1.HasOTel)

	web2 := res.Compute[byID["web-2"]]
	assert.Equal(t, "e2-medium", web2.InstanceType, "URL-style machineType trims correctly")
	assert.True(t, web2.HasOTel)

	worker := res.Compute[byID["worker-1"]]
	assert.Equal(t, "us-west1", worker.Region)

	// Walked-regions accounting.
	sort.Strings(res.Regions)
	assert.Equal(t, []string{"us-central1", "us-west1"}, res.Regions)
}

func TestScan_HasOTelTrueForOtelLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"lowercase otel prefix", map[string]string{"otel": "v1"}},
		{"otel-collector compound", map[string]string{"otel-collector": "v1", "env": "prod"}},
		{"OTEL uppercase prefix", map[string]string{"OTEL_AGENT": "v1"}},
		{"mixed-case", map[string]string{"Otel": "v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGCP()
			fake.Zones = []*compute.Zone{{Name: "us-central1-a"}}
			fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
				{Name: "inst", MachineType: "zones/us-central1-a/machineTypes/n1-standard-1", Labels: tc.labels},
			}
			s := newScannerWithFake(t, fake, "p", "")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.True(t, res.Compute[0].HasOTel, "expected HasOTel=true for label %v", tc.labels)
			assert.Equal(t, 1, res.InstrumentedCount)
			assert.Equal(t, 0, res.UninstrumentedCount)
		})
	}
}

func TestScan_HasOTelFalseForNoOtelLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"no labels", nil},
		{"empty labels", map[string]string{}},
		{"non-otel labels", map[string]string{"env": "prod", "team": "platform"}},
		{"close-but-not-prefix", map[string]string{"telemetry": "on", "monitoring": "yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGCP()
			fake.Zones = []*compute.Zone{{Name: "us-central1-a"}}
			fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
				{Name: "inst", MachineType: "zones/us-central1-a/machineTypes/n1-standard-1", Labels: tc.labels},
			}
			s := newScannerWithFake(t, fake, "p", "")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.False(t, res.Compute[0].HasOTel, "expected HasOTel=false for labels %v", tc.labels)
			assert.Equal(t, 0, res.InstrumentedCount)
			assert.Equal(t, 1, res.UninstrumentedCount)
		})
	}
}

func TestScan_RegionFilterRestrictsZones(t *testing.T) {
	fake := newFakeGCP()
	fake.Zones = []*compute.Zone{
		{Name: "us-central1-a"},
		{Name: "us-central1-b"},
		{Name: "us-west1-a"},
	}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "central-a-1", MachineType: "zones/us-central1-a/machineTypes/e2-medium"},
	}
	fake.InstancesByZone["us-central1-b"] = []*compute.Instance{
		{Name: "central-b-1", MachineType: "zones/us-central1-b/machineTypes/e2-medium"},
	}
	fake.InstancesByZone["us-west1-a"] = []*compute.Instance{
		{Name: "west-a-1", MachineType: "zones/us-west1-a/machineTypes/e2-medium"},
	}

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	// Only us-central1-* zones should be walked.
	require.Len(t, res.Compute, 2)
	ids := map[string]struct{}{}
	for _, c := range res.Compute {
		ids[c.ResourceID] = struct{}{}
		assert.Equal(t, "us-central1", c.Region)
	}
	assert.Contains(t, ids, "central-a-1")
	assert.Contains(t, ids, "central-b-1")
	assert.NotContains(t, ids, "west-a-1")

	// Only us-central1 should be in the walked-regions list.
	assert.Equal(t, []string{"us-central1"}, res.Regions)

	// Confirm the mock was NOT called for us-west1-a (the scanner
	// filtered it out before issuing the instances.list call).
	assert.Equal(t, 0, fake.InstancesListCalls["us-west1-a"])
	assert.Equal(t, 1, fake.InstancesListCalls["us-central1-a"])
	assert.Equal(t, 1, fake.InstancesListCalls["us-central1-b"])
}

func TestScan_RateLimitMidWalk_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.Zones = []*compute.Zone{
		{Name: "us-central1-a"},
		{Name: "us-west1-a"},
	}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "good-1", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel": "v1"}},
	}
	// us-west1-a returns 429 — the second zone fails mid-walk.
	fake.InstancesListStatusByZone["us-west1-a"] = http.StatusTooManyRequests

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "partial failures return nil error; the first zone's success matters")

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDComputeEngine)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDComputeEngine)

	// The first zone's instance must still be present — partial does
	// NOT mean "throw away the successful walk."
	require.Len(t, res.Compute, 1)
	assert.Equal(t, "good-1", res.Compute[0].ResourceID)
	assert.Equal(t, 1, res.InstrumentedCount)
	assert.Equal(t, 0, res.UninstrumentedCount)
}

func TestScan_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.ZonesListStatus = http.StatusForbidden

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied at zones list is a partial-failure surface, not a hard error")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "roles/compute.viewer")
	assert.Contains(t, res.FailedServices, ServiceIDComputeEngine)
	assert.Empty(t, res.Compute)
}

func TestScan_ProjectNotFound_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.ZonesListStatus = http.StatusNotFound

	s := newScannerWithFake(t, fake, "missing-project", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "project not found")
	assert.Contains(t, res.PartialReason, "project_id")
	assert.Contains(t, res.FailedServices, ServiceIDComputeEngine)
	assert.Empty(t, res.Compute)
}

func TestScan_NetworkError_RecordsPartialFailure(t *testing.T) {
	// Pick a free port, close it, and point the scanner at the
	// dead address. The connection attempt fails with "connection
	// refused" (or equivalent) — a transport-layer error that
	// surfaces as the network branch of classifyZonesListError.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	s := &Scanner{
		ProjectID:  "p",
		SAJSON:     nil,
		httpClient: &http.Client{Timeout: 2 * time.Second},
		endpoint:   "http://" + addr,
	}
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "transport failures are partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "network")
	assert.Contains(t, res.FailedServices, ServiceIDComputeEngine)
}

func TestScan_InstrumentedCountMatchesHasOTelTrue(t *testing.T) {
	fake := newFakeGCP()
	fake.Zones = []*compute.Zone{{Name: "us-central1-a"}}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "a", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel": "v1"}},
		{Name: "b", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel-collector": "v1"}},
		{Name: "c", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"env": "prod"}},
		{Name: "d", MachineType: "zones/us-central1-a/machineTypes/e2-medium"},
		{Name: "e", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"team": "data"}},
	}

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 5)
	assert.Equal(t, 2, res.InstrumentedCount)
	assert.Equal(t, 3, res.UninstrumentedCount)
}

// TestScan_RequiresProjectID confirms the empty-ProjectID branch
// returns a hard error rather than emitting a successful but
// nonsense scan (project_id is the substrate's primary identifier;
// missing it is a caller bug).
func TestScan_RequiresProjectID(t *testing.T) {
	s := &Scanner{
		// No ProjectID, no SAJSON, no httpClient.
	}
	_, err := s.Scan(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ProjectID")
}

// TestScan_RequiresAuth confirms the empty-SAJSON-and-no-httpClient
// branch returns a hard error. The httpClient path is the test
// bypass; the SAJSON path is the production path. Both missing is a
// misconfiguration the constructor should catch.
func TestScan_RequiresAuth(t *testing.T) {
	s := &Scanner{
		ProjectID: "p",
	}
	_, err := s.Scan(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SAJSON")
}

// TestProvider_ReturnsGCP confirms the scanner reports itself as the
// GCP provider — chunk 3's API trampoline branches on this to dispatch
// to the right scanner per CloudConnection.
func TestProvider_ReturnsGCP(t *testing.T) {
	s := &Scanner{}
	assert.Equal(t, credstore.ProviderGCP, s.Provider())
}

// TestRegionFromZone exercises the zone→region helper directly. The
// production scanner exercises it implicitly via the per-instance
// Region field, but the helper is shared territory worth a dedicated
// test.
func TestRegionFromZone(t *testing.T) {
	cases := []struct {
		zone, region string
	}{
		{"us-central1-a", "us-central1"},
		{"us-central1-b", "us-central1"},
		{"europe-west4-c", "europe-west4"},
		{"asia-northeast1-a", "asia-northeast1"},
		{"", ""},
		{"weirdshape", "weirdshape"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.region, regionFromZone(tc.zone), "zone=%q", tc.zone)
	}
}

// TestTrimMachineType exercises both URL shapes the API returns.
func TestTrimMachineType(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"zones/us-central1-a/machineTypes/n2-standard-4", "n2-standard-4"},
		{"https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/machineTypes/e2-medium", "e2-medium"},
		{"n2-standard-4", "n2-standard-4"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.out, trimMachineType(tc.in), "in=%q", tc.in)
	}
}
