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
	cloudfunctions "google.golang.org/api/cloudfunctions/v1"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1beta1"
	run "google.golang.org/api/run/v1"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	storage "google.golang.org/api/storage/v1"

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

	// Buckets is the static GCS bucket list served by buckets.list
	// (object-store tier — coverage-parity arc).
	Buckets []*storage.Bucket
	// BucketsListStatus, when non-zero, makes buckets.list return it.
	BucketsListStatus int

	// InstancesByZone maps zone name → instances list response. The
	// mock returns an empty list when a zone isn't seeded.
	InstancesByZone map[string][]*compute.Instance

	// ZonesListStatus, when non-zero, makes the next zones.list call
	// return this status (with a googleapi-style body).
	ZonesListStatus int

	// InstancesListStatusByZone, when set for a zone, makes the
	// instances.list call for that zone return the configured status.
	InstancesListStatusByZone map[string]int

	// Cloud SQL (slice 2) test surface.
	//
	// CloudSQLInstances is the flat list returned when no pagination is
	// configured. The mock returns an empty list when neither
	// CloudSQLInstances nor CloudSQLPages is set.
	CloudSQLInstances []*sqladmin.DatabaseInstance

	// CloudSQLPages, when non-nil, makes the Cloud SQL list endpoint
	// page through the supplied page sequence. Each page returns its
	// instances and a NextPageToken pointing at the next page (or
	// empty for the last page). The mock pulls the page by index
	// based on the inbound pageToken query param.
	CloudSQLPages [][]*sqladmin.DatabaseInstance

	// CloudSQLListStatus, when non-zero, makes the next Cloud SQL
	// instances.list call return this status.
	CloudSQLListStatus int

	// GKE (slice 2 kubernetes-tier) test surface.
	//
	// GKEClusters is the static cluster list returned by the
	// v1beta1 ProjectsLocationsClustersService.List endpoint. The
	// mock returns an empty list when no clusters are seeded.
	GKEClusters []*container.Cluster

	// GKEListStatus, when non-zero, makes the next GKE
	// clusters.list call return this status (with a googleapi-style
	// body).
	GKEListStatus int

	// Cloud Run (serverless tier slice 1 chunk 2) test surface.
	//
	// CloudRunServicesByRegion maps region → services list returned
	// by the /v1/projects/.../locations/{region}/services endpoint.
	// When unset for a region, the mock returns an empty list (the
	// scanner's per-region walker still records the empty result).
	CloudRunServicesByRegion map[string][]*run.Service

	// CloudRunPagesByRegion, when set for a region, makes the
	// Cloud Run List endpoint paginate via Knative continuation
	// tokens. Each page returns its services and a Metadata.Continue
	// pointing at the next page. The mock pulls the page by index
	// based on the inbound "continue" query param.
	CloudRunPagesByRegion map[string][][]*run.Service

	// CloudRunLocations is the list of locations the
	// Projects.Locations.List call returns when the scanner needs
	// to enumerate regions (s.Region empty). When nil the mock
	// returns an empty list.
	CloudRunLocations []*run.Location

	// CloudRunListStatus, when non-zero, makes the next Cloud Run
	// Services.List call return this status.
	CloudRunListStatus int

	// CloudRunLocationsStatus, when non-zero, makes the next
	// Projects.Locations.List call (run/v1) return this status.
	CloudRunLocationsStatus int

	// Cloud Functions (serverless tier slice 1 chunk 2) test surface.
	//
	// CloudFunctions is the static list returned by the
	// /v1/projects/.../locations/-/functions endpoint. Tests seed
	// fn.Name with the canonical "projects/{p}/locations/{r}/functions/{n}"
	// shape so the scanner's region projection lands honestly.
	CloudFunctions []*cloudfunctions.CloudFunction

	// CloudFunctionsPages, when non-nil, makes the Cloud Functions
	// list endpoint paginate via the standard nextPageToken.
	CloudFunctionsPages [][]*cloudfunctions.CloudFunction

	// CloudFunctionsListStatus, when non-zero, makes the next
	// Cloud Functions list call return this status.
	CloudFunctionsListStatus int

	// Call counters for assertions.
	ZonesListCalls          int
	InstancesListCalls      map[string]int
	CloudSQLListCalls       int
	GKEListCalls            int
	CloudRunListCalls       map[string]int
	CloudRunLocationsCalls  int
	CloudFunctionsListCalls int
}

func newFakeGCP() *fakeGCP {
	return &fakeGCP{
		InstancesByZone:           map[string][]*compute.Instance{},
		InstancesListStatusByZone: map[string]int{},
		InstancesListCalls:        map[string]int{},
		CloudRunServicesByRegion:  map[string][]*run.Service{},
		CloudRunPagesByRegion:     map[string][][]*run.Service{},
		CloudRunListCalls:         map[string]int{},
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
		// Cloud SQL Admin API path shape (REST, v1beta4):
		//   /sql/v1beta4/projects/{project}/instances
		// GKE Container API path shape (REST, v1beta1):
		//   /v1beta1/projects/{project}/locations/-/clusters
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

		case strings.Contains(path, "/sql/v1beta4/projects/") && strings.HasSuffix(path, "/instances"):
			f.CloudSQLListCalls++
			if f.CloudSQLListStatus != 0 {
				writeAPIError(w, f.CloudSQLListStatus, statusReason(f.CloudSQLListStatus), statusName(f.CloudSQLListStatus))
				return
			}
			items, nextToken := f.cloudSQLPage(r.URL.Query().Get("pageToken"))
			resp := sqladmin.InstancesListResponse{
				Items:         items,
				Kind:          "sql#instancesList",
				NextPageToken: nextToken,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return

		case strings.Contains(path, "/v1beta1/projects/") && strings.HasSuffix(path, "/clusters"):
			// GKE Container API: clusters.list with the "-" location
			// wildcard. The parent path is "projects/{p}/locations/-",
			// so the URL ends in "/locations/-/clusters". The mock
			// returns every seeded cluster on a single response
			// (the API doesn't expose a NextPageToken on
			// ListClustersResponse — see gke.go::walkGKE godoc).
			f.GKEListCalls++
			if f.GKEListStatus != 0 {
				writeAPIError(w, f.GKEListStatus, statusReason(f.GKEListStatus), statusName(f.GKEListStatus))
				return
			}
			resp := container.ListClustersResponse{Clusters: f.GKEClusters}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return

		case strings.Contains(path, "/v1/projects/") && strings.Contains(path, "/locations/") && strings.HasSuffix(path, "/services"):
			// Cloud Run Admin API path shape (REST, v1):
			//   /v1/projects/{project}/locations/{region}/services
			// The per-region walker hits this once per region; the
			// fake honors per-region seed lists and per-region paging.
			region := parseRegionFromServicesPath(path)
			f.CloudRunListCalls[region]++
			if f.CloudRunListStatus != 0 {
				writeAPIError(w, f.CloudRunListStatus, statusReason(f.CloudRunListStatus), statusName(f.CloudRunListStatus))
				return
			}
			items, nextToken := f.cloudRunPage(region, r.URL.Query().Get("continue"))
			resp := run.ListServicesResponse{
				Items: items,
				Kind:  "ServiceList",
			}
			if nextToken != "" {
				resp.Metadata = &run.ListMeta{Continue: nextToken}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return

		case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/locations"):
			// Cloud Run Projects.Locations.List path shape (REST, v1):
			//   /v1/projects/{project}/locations
			// The scanner calls this when s.Region is empty to
			// enumerate locations before fanning Services.List per
			// region. The fake honors CloudRunLocations directly.
			f.CloudRunLocationsCalls++
			if f.CloudRunLocationsStatus != 0 {
				writeAPIError(w, f.CloudRunLocationsStatus, statusReason(f.CloudRunLocationsStatus), statusName(f.CloudRunLocationsStatus))
				return
			}
			resp := run.ListLocationsResponse{Locations: f.CloudRunLocations}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return

		case strings.Contains(path, "/v1/projects/") && strings.HasSuffix(path, "/functions"):
			// Cloud Functions API path shape (REST, v1):
			//   /v1/projects/{project}/locations/-/functions
			// The scanner uses the "-" location wildcard; the fake
			// honors the flat list or the paged-list seed.
			f.CloudFunctionsListCalls++
			if f.CloudFunctionsListStatus != 0 {
				writeAPIError(w, f.CloudFunctionsListStatus, statusReason(f.CloudFunctionsListStatus), statusName(f.CloudFunctionsListStatus))
				return
			}
			items, nextToken := f.cloudFunctionsPage(r.URL.Query().Get("pageToken"))
			resp := cloudfunctions.ListFunctionsResponse{
				Functions:     items,
				NextPageToken: nextToken,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		case strings.HasSuffix(path, "/b"):
			// GCS Buckets.List path shape: {base}/b?project=...
			// handler() already holds f.mu (locked at the top), so read
			// the seed fields directly — re-locking would deadlock.
			if f.BucketsListStatus != 0 {
				writeAPIError(w, f.BucketsListStatus, statusReason(f.BucketsListStatus), statusName(f.BucketsListStatus))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(storage.Buckets{Items: f.Buckets})
			return
		}

		// Unmatched path — surface as 404 so test failures are
		// obvious (rather than the scanner silently consuming an
		// empty body).
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path), "NOT_FOUND")
	})
}

// parseRegionFromServicesPath extracts the region segment from a Cloud
// Run services-list URL of shape ".../locations/{region}/services".
func parseRegionFromServicesPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// cloudRunPage resolves the inbound "continue" token into the (items,
// nextToken) pair the mock should return for the given region.
func (f *fakeGCP) cloudRunPage(region, continueToken string) ([]*run.Service, string) {
	pages, ok := f.CloudRunPagesByRegion[region]
	if !ok || len(pages) == 0 {
		return f.CloudRunServicesByRegion[region], ""
	}
	idx := 0
	if continueToken != "" {
		var n int
		_, _ = fmt.Sscanf(continueToken, "page-%d", &n)
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

// cloudFunctionsPage resolves the inbound pageToken into the (items,
// nextToken) pair the mock should return.
func (f *fakeGCP) cloudFunctionsPage(pageToken string) ([]*cloudfunctions.CloudFunction, string) {
	if len(f.CloudFunctionsPages) == 0 {
		return f.CloudFunctions, ""
	}
	idx := 0
	if pageToken != "" {
		var n int
		_, _ = fmt.Sscanf(pageToken, "page-%d", &n)
		idx = n
	}
	if idx >= len(f.CloudFunctionsPages) {
		return nil, ""
	}
	items := f.CloudFunctionsPages[idx]
	if idx+1 < len(f.CloudFunctionsPages) {
		return items, fmt.Sprintf("page-%d", idx+1)
	}
	return items, ""
}

// cloudSQLPage resolves the inbound pageToken into the (items, nextToken)
// pair the mock should return. The token shape is "page-N" (1-indexed);
// the empty token selects page 0 (or the flat CloudSQLInstances list
// when no pages are configured).
func (f *fakeGCP) cloudSQLPage(pageToken string) ([]*sqladmin.DatabaseInstance, string) {
	if len(f.CloudSQLPages) == 0 {
		return f.CloudSQLInstances, ""
	}
	idx := 0
	if pageToken != "" {
		// Parse "page-N" -> N. Default to 0 on parse failure (defensive).
		var n int
		_, _ = fmt.Sscanf(pageToken, "page-%d", &n)
		idx = n
	}
	if idx >= len(f.CloudSQLPages) {
		return nil, ""
	}
	items := f.CloudSQLPages[idx]
	if idx+1 < len(f.CloudSQLPages) {
		return items, fmt.Sprintf("page-%d", idx+1)
	}
	return items, ""
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

// --- Slice 2 Cloud SQL tests -----------------------------------------
//
// These tests exercise the database-tier-slice2.md §3.1 detection rule
// and the §5.1 scanner extension. The mock surface is the same
// fakeGCP server — Cloud SQL paths route through the
// /sql/v1beta4/projects/.../instances handler added alongside the
// zones / instances handlers.

// cloudSQLInstance is a tiny helper for assembling test instances
// without verbose struct nesting. Tests that need richer fields fall
// through to the raw sqladmin.DatabaseInstance literal.
func cloudSQLInstance(name, databaseVersion, region, tier string, queryInsights bool) *sqladmin.DatabaseInstance {
	return &sqladmin.DatabaseInstance{
		Name:            name,
		DatabaseVersion: databaseVersion,
		Region:          region,
		Settings: &sqladmin.Settings{
			Tier: tier,
			InsightsConfig: &sqladmin.InsightsConfig{
				QueryInsightsEnabled: queryInsights,
			},
		},
	}
}

func TestScan_CloudSQL_ReturnsDatabaseInstanceSnapshot(t *testing.T) {
	fake := newFakeGCP()
	// Empty zones list — the Cloud SQL walk runs even when compute
	// returned no instances.
	fake.Zones = []*compute.Zone{}
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		cloudSQLInstance("db-postgres-1", "POSTGRES_15", "us-central1", "db-custom-2-7680", true),
		cloudSQLInstance("db-mysql-1", "MYSQL_8_0", "us-east1", "db-n1-standard-2", false),
		cloudSQLInstance("db-sqlserver-1", "SQLSERVER_2019_STANDARD", "europe-west1", "db-custom-4-15360", true),
	}

	s := newScannerWithFake(t, fake, "test-project", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 3)
	assert.Equal(t, 1, fake.CloudSQLListCalls)

	byID := map[string]int{}
	for i, d := range res.Databases {
		byID[d.ResourceID] = i
	}
	require.Contains(t, byID, "db-postgres-1")
	require.Contains(t, byID, "db-mysql-1")
	require.Contains(t, byID, "db-sqlserver-1")

	pg := res.Databases[byID["db-postgres-1"]]
	assert.Equal(t, "postgres", pg.Engine)
	assert.Equal(t, "15", pg.EngineVersion)
	assert.Equal(t, "db-custom-2-7680", pg.InstanceClass)
	assert.Equal(t, "us-central1", pg.Region)
	assert.Equal(t, "gcp", pg.Provider)
	assert.True(t, pg.QueryInsightsEnabled)

	mysql := res.Databases[byID["db-mysql-1"]]
	assert.Equal(t, "mysql", mysql.Engine)
	assert.Equal(t, "8_0", mysql.EngineVersion)
	assert.False(t, mysql.QueryInsightsEnabled)

	mssql := res.Databases[byID["db-sqlserver-1"]]
	assert.Equal(t, "sqlserver", mssql.Engine)
	assert.Equal(t, "2019_STANDARD", mssql.EngineVersion)
	assert.True(t, mssql.QueryInsightsEnabled)
}

func TestScan_CloudSQL_QueryInsightsEnabledDetection(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		cloudSQLInstance("db-on", "POSTGRES_15", "us-central1", "db-custom-2-7680", true),
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 1)
	assert.True(t, res.Databases[0].QueryInsightsEnabled)
	// Slice 2 instrumented rule: QueryInsightsEnabled=true counts.
	assert.Equal(t, 1, res.InstrumentedCount)
	assert.Equal(t, 0, res.UninstrumentedCount)
}

func TestScan_CloudSQL_QueryInsightsMissing_TreatsAsFalse(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		// Settings present but InsightsConfig nil.
		{
			Name:            "no-insights-cfg",
			DatabaseVersion: "POSTGRES_14",
			Region:          "us-central1",
			Settings: &sqladmin.Settings{
				Tier: "db-custom-1-3840",
			},
		},
		// Settings nil entirely.
		{
			Name:            "no-settings",
			DatabaseVersion: "MYSQL_5_7",
			Region:          "us-west1",
		},
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 2)
	for _, d := range res.Databases {
		assert.False(t, d.QueryInsightsEnabled, "missing insightsConfig should default to false (resource=%s)", d.ResourceID)
	}
	// Both rows uninstrumented per the slice 2 rule.
	assert.Equal(t, 0, res.InstrumentedCount)
	assert.Equal(t, 2, res.UninstrumentedCount)
}

func TestScan_CloudSQL_EngineNormalization(t *testing.T) {
	cases := []struct {
		databaseVersion string
		engine          string
		engineVersion   string
	}{
		{"POSTGRES_15", "postgres", "15"},
		{"POSTGRES_14", "postgres", "14"},
		{"POSTGRES_9_6", "postgres", "9_6"},
		{"MYSQL_8_0", "mysql", "8_0"},
		{"MYSQL_5_7", "mysql", "5_7"},
		{"MYSQL_8_4", "mysql", "8_4"},
		{"SQLSERVER_2019_STANDARD", "sqlserver", "2019_STANDARD"},
		{"SQLSERVER_2017_ENTERPRISE", "sqlserver", "2017_ENTERPRISE"},
		// Unknown family — passthrough lowercase, empty version.
		{"WEIRD_NEW_ENGINE", "weird_new_engine", "NEW_ENGINE"},
		{"", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.databaseVersion, func(t *testing.T) {
			assert.Equal(t, tc.engine, normalizeEngine(tc.databaseVersion))
			assert.Equal(t, tc.engineVersion, extractVersion(tc.databaseVersion))
		})
	}
}

func TestScan_CloudSQL_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	// Seed a successful compute walk so the test confirms compute
	// results survive a cloudsql failure.
	fake.Zones = []*compute.Zone{{Name: "us-central1-a"}}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "vm-good", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel": "v1"}},
	}
	fake.CloudSQLListStatus = http.StatusForbidden

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "cloudsql 403 is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDCloudSQL)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "roles/cloudsql.viewer")
	assert.Contains(t, res.FailedServices, ServiceIDCloudSQL)

	// Compute walk still produced its result.
	require.Len(t, res.Compute, 1)
	assert.Equal(t, "vm-good", res.Compute[0].ResourceID)
	assert.Empty(t, res.Databases)
}

func TestScan_CloudSQL_RateLimit_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLListStatus = http.StatusTooManyRequests

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDCloudSQL)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDCloudSQL)
}

func TestScan_CloudSQL_ProjectNotFound_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLListStatus = http.StatusNotFound

	s := newScannerWithFake(t, fake, "missing-project", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "project not found")
	assert.Contains(t, res.PartialReason, "project_id")
	assert.Contains(t, res.FailedServices, ServiceIDCloudSQL)
}

func TestScan_CloudSQL_Pagination(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLPages = [][]*sqladmin.DatabaseInstance{
		{
			cloudSQLInstance("db-a-1", "POSTGRES_15", "us-central1", "db-custom-1-3840", false),
			cloudSQLInstance("db-a-2", "POSTGRES_15", "us-central1", "db-custom-1-3840", false),
		},
		{
			cloudSQLInstance("db-b-1", "POSTGRES_15", "us-central1", "db-custom-1-3840", true),
			cloudSQLInstance("db-b-2", "POSTGRES_15", "us-central1", "db-custom-1-3840", false),
		},
		{
			cloudSQLInstance("db-c-1", "POSTGRES_15", "us-central1", "db-custom-1-3840", false),
			cloudSQLInstance("db-c-2", "POSTGRES_15", "us-central1", "db-custom-1-3840", true),
		},
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 6)
	assert.Equal(t, 3, fake.CloudSQLListCalls, "pagination should produce 3 list calls")

	ids := map[string]struct{}{}
	for _, d := range res.Databases {
		ids[d.ResourceID] = struct{}{}
	}
	for _, want := range []string{"db-a-1", "db-a-2", "db-b-1", "db-b-2", "db-c-1", "db-c-2"} {
		assert.Contains(t, ids, want)
	}
	// Instrumented tally: db-b-1, db-c-2 (2 enabled, 4 disabled).
	assert.Equal(t, 2, res.InstrumentedCount)
	assert.Equal(t, 4, res.UninstrumentedCount)
}

func TestScan_CloudSQL_TagsFromUserLabels(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		{
			Name:            "tagged",
			DatabaseVersion: "POSTGRES_15",
			Region:          "us-central1",
			Settings: &sqladmin.Settings{
				Tier: "db-custom-1-3840",
				UserLabels: map[string]string{
					"env":  "prod",
					"team": "platform",
				},
				InsightsConfig: &sqladmin.InsightsConfig{QueryInsightsEnabled: true},
			},
		},
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 1)
	assert.Equal(t, map[string]string{"env": "prod", "team": "platform"}, res.Databases[0].Tags)
}

func TestScan_ComputeAndCloudSQL_BothWalked(t *testing.T) {
	fake := newFakeGCP()
	fake.Zones = []*compute.Zone{
		{Name: "us-central1-a"},
		{Name: "us-west1-a"},
	}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "vm-1", MachineType: "zones/us-central1-a/machineTypes/e2-medium"},
		{Name: "vm-2", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel": "v1"}},
	}
	fake.InstancesByZone["us-west1-a"] = []*compute.Instance{
		{Name: "vm-3", MachineType: "zones/us-west1-a/machineTypes/e2-medium"},
		{Name: "vm-4", MachineType: "zones/us-west1-a/machineTypes/e2-medium"},
	}
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		cloudSQLInstance("db-1", "POSTGRES_15", "us-central1", "db-custom-1-3840", true),
		cloudSQLInstance("db-2", "MYSQL_8_0", "us-west1", "db-n1-standard-2", false),
	}

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.Len(t, res.Compute, 4)
	assert.Len(t, res.Databases, 2)
	assert.False(t, res.Partial)
	// Tally: compute (1 otel) + databases (1 query insights) = 2
	// instrumented, 4 compute uninstrumented + 1 database = 5 total.
	assert.Equal(t, 2, res.InstrumentedCount)
	assert.Equal(t, 4, res.UninstrumentedCount)
}

func TestScan_CloudSQL_ProviderFieldSet(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		cloudSQLInstance("a", "POSTGRES_15", "us-central1", "db-custom-1-3840", true),
		cloudSQLInstance("b", "MYSQL_8_0", "us-east1", "db-n1-standard-2", false),
		cloudSQLInstance("c", "SQLSERVER_2019_STANDARD", "europe-west1", "db-custom-4-15360", true),
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 3)
	for _, d := range res.Databases {
		assert.Equal(t, "gcp", d.Provider, "resource=%s should have Provider=gcp", d.ResourceID)
	}
}

func TestScan_CloudSQL_RegionFilter(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		cloudSQLInstance("central-1", "POSTGRES_15", "us-central1", "db-custom-1-3840", true),
		cloudSQLInstance("central-2", "POSTGRES_15", "us-central1", "db-custom-1-3840", false),
		cloudSQLInstance("east-1", "POSTGRES_15", "us-east1", "db-custom-1-3840", true),
		cloudSQLInstance("west-1", "POSTGRES_15", "us-west1", "db-custom-1-3840", true),
	}
	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	// Region filter applies client-side after the full list arrives.
	require.Len(t, res.Databases, 2)
	ids := map[string]struct{}{}
	for _, d := range res.Databases {
		ids[d.ResourceID] = struct{}{}
		assert.Equal(t, "us-central1", d.Region)
	}
	assert.Contains(t, ids, "central-1")
	assert.Contains(t, ids, "central-2")
	assert.NotContains(t, ids, "east-1")
	assert.NotContains(t, ids, "west-1")
}

func TestNormalizeEngineAndExtractVersion_Standalone(t *testing.T) {
	// Direct exercise of the helpers; the table test above covers
	// these via Scan but the standalone form helps a debugger
	// pinpoint a regression to the helper rather than the walker.
	assert.Equal(t, "postgres", normalizeEngine("POSTGRES_15"))
	assert.Equal(t, "mysql", normalizeEngine("MYSQL_8_0"))
	assert.Equal(t, "sqlserver", normalizeEngine("SQLSERVER_2019_STANDARD"))
	assert.Equal(t, "15", extractVersion("POSTGRES_15"))
	assert.Equal(t, "", extractVersion(""))
	assert.Equal(t, "", extractVersion("POSTGRES"))
}

// --- Slice 2 GKE tests -----------------------------------------------
//
// These tests exercise the kubernetes-tier-slice2.md §3.1 detection
// rule and the §5.1 scanner extension. The mock surface is the same
// fakeGCP server — GKE paths route through the
// /v1beta1/projects/.../locations/-/clusters handler added alongside
// the zones / instances / Cloud SQL handlers.

// gkeCluster is a tiny helper for assembling test clusters without
// verbose struct nesting. The ManagedPrometheus boolean is hoisted
// into the convenience signature since it's the slice-2 detection
// axis the proposer keys on. Tests that need a missing
// MonitoringConfig (or a missing ManagedPrometheusConfig under a
// present MonitoringConfig) fall through to the raw container.Cluster
// literal — see TestScan_GKE_ManagedPrometheusMissing_TreatsAsFalse.
func gkeCluster(name, masterVersion, location, status string, managedPrometheus bool, labels map[string]string) *container.Cluster {
	return &container.Cluster{
		Name:                 name,
		SelfLink:             "https://container.googleapis.com/v1beta1/projects/p/locations/" + location + "/clusters/" + name,
		CurrentMasterVersion: masterVersion,
		Status:               status,
		Location:             location,
		ResourceLabels:       labels,
		MonitoringConfig: &container.MonitoringConfig{
			ManagedPrometheusConfig: &container.ManagedPrometheusConfig{
				Enabled: managedPrometheus,
			},
		},
	}
}

func TestScan_GKE_ReturnsClusterSnapshot(t *testing.T) {
	fake := newFakeGCP()
	// Empty zones list — the GKE walk runs even when compute returned
	// no instances and no Cloud SQL instances were seeded.
	fake.Zones = []*compute.Zone{}
	fake.GKEClusters = []*container.Cluster{
		gkeCluster("prod-cluster", "1.29.4-gke.1043000", "us-central1", "RUNNING", true, map[string]string{"env": "prod"}),
		gkeCluster("staging-cluster", "1.30.1-gke.500", "us-west1", "RUNNING", false, map[string]string{"env": "staging"}),
	}

	s := newScannerWithFake(t, fake, "test-project", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 2)
	assert.Equal(t, 1, fake.GKEListCalls)

	byName := map[string]int{}
	for i, c := range res.Clusters {
		byName[c.Name] = i
	}
	require.Contains(t, byName, "prod-cluster")
	require.Contains(t, byName, "staging-cluster")

	prod := res.Clusters[byName["prod-cluster"]]
	assert.Equal(t, "https://container.googleapis.com/v1beta1/projects/p/locations/us-central1/clusters/prod-cluster", prod.ResourceID)
	assert.Equal(t, "1.29", prod.KubernetesVersion)
	assert.Equal(t, "RUNNING", prod.Status)
	assert.Equal(t, "us-central1", prod.Region)
	assert.Equal(t, "gcp", prod.Provider)
	assert.True(t, prod.ManagedPrometheusEnabled)
	assert.Equal(t, map[string]string{"env": "prod"}, prod.Tags)
	// AWS-specific fields must be untouched on GCP-projected snapshots.
	assert.Empty(t, prod.ControlPlaneLogging)
	assert.Empty(t, prod.Addons)
	assert.Zero(t, prod.NodegroupCount)
	assert.Zero(t, prod.FargateProfileCount)

	staging := res.Clusters[byName["staging-cluster"]]
	assert.Equal(t, "1.30", staging.KubernetesVersion)
	assert.False(t, staging.ManagedPrometheusEnabled)
}

func TestScan_GKE_ManagedPrometheusEnabledDetection(t *testing.T) {
	fake := newFakeGCP()
	fake.GKEClusters = []*container.Cluster{
		gkeCluster("mp-on", "1.29.4-gke.1043000", "us-central1", "RUNNING", true, nil),
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1)
	assert.True(t, res.Clusters[0].ManagedPrometheusEnabled)
	// Slice 2 instrumented rule: ManagedPrometheusEnabled=true counts.
	assert.Equal(t, 1, res.InstrumentedCount)
	assert.Equal(t, 0, res.UninstrumentedCount)
}

func TestScan_GKE_ManagedPrometheusMissing_TreatsAsFalse(t *testing.T) {
	fake := newFakeGCP()
	fake.GKEClusters = []*container.Cluster{
		// MonitoringConfig present but ManagedPrometheusConfig nil.
		{
			Name:                 "no-mp-cfg",
			SelfLink:             "https://container.googleapis.com/v1beta1/projects/p/locations/us-central1/clusters/no-mp-cfg",
			CurrentMasterVersion: "1.29.4-gke.1043000",
			Status:               "RUNNING",
			Location:             "us-central1",
			MonitoringConfig: &container.MonitoringConfig{
				// Only the componentConfig is set; managedPrometheusConfig
				// is intentionally nil so the detection rule's nil-safety
				// branch fires (design doc §3.1: missing
				// managedPrometheusConfig reads as enabled=false).
				ComponentConfig: &container.MonitoringComponentConfig{
					EnableComponents: []string{"SYSTEM_COMPONENTS"},
				},
			},
		},
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].ManagedPrometheusEnabled, "missing managedPrometheusConfig should default to false")
	assert.Equal(t, 0, res.InstrumentedCount)
	assert.Equal(t, 1, res.UninstrumentedCount)
}

func TestScan_GKE_MonitoringConfigMissing_TreatsAsFalse(t *testing.T) {
	fake := newFakeGCP()
	fake.GKEClusters = []*container.Cluster{
		// MonitoringConfig entirely nil — a pre-managed-observability
		// cluster shape. The detection rule's outermost nil-safety
		// branch fires.
		{
			Name:                 "legacy",
			SelfLink:             "https://container.googleapis.com/v1beta1/projects/p/locations/us-central1/clusters/legacy",
			CurrentMasterVersion: "1.28.7-gke.1100000",
			Status:               "RUNNING",
			Location:             "us-central1",
		},
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].ManagedPrometheusEnabled, "missing monitoringConfig should default to false")
	assert.Equal(t, 0, res.InstrumentedCount)
	assert.Equal(t, 1, res.UninstrumentedCount)
}

func TestScan_GKE_VersionExtraction(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"1.29.4-gke.1043000", "1.29"},
		{"1.30.1-gke.500", "1.30"},
		{"1.28", "1.28"},
		{"1.29.4", "1.29"},
		{"", ""},
		{"weirdshape", "weirdshape"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.out, extractMajorMinor(tc.in))
		})
	}
}

func TestScan_GKE_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	// Seed a successful compute walk so the test confirms compute
	// results survive a GKE failure.
	fake.Zones = []*compute.Zone{{Name: "us-central1-a"}}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "vm-good", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel": "v1"}},
	}
	fake.GKEListStatus = http.StatusForbidden

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "gke 403 is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDGKE)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "roles/container.viewer")
	assert.Contains(t, res.FailedServices, ServiceIDGKE)

	// Compute walk still produced its result.
	require.Len(t, res.Compute, 1)
	assert.Equal(t, "vm-good", res.Compute[0].ResourceID)
	assert.Empty(t, res.Clusters)
}

func TestScan_GKE_RateLimit_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.GKEListStatus = http.StatusTooManyRequests

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDGKE)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDGKE)
}

func TestScan_GKE_ProviderFieldSet(t *testing.T) {
	fake := newFakeGCP()
	fake.GKEClusters = []*container.Cluster{
		gkeCluster("a", "1.29.4-gke.1043000", "us-central1", "RUNNING", true, nil),
		gkeCluster("b", "1.30.1-gke.500", "us-east1", "RUNNING", false, nil),
		gkeCluster("c", "1.28.7-gke.1100000", "europe-west1", "RUNNING", true, nil),
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 3)
	for _, c := range res.Clusters {
		assert.Equal(t, "gcp", c.Provider, "cluster=%s should have Provider=gcp", c.Name)
	}
}

func TestScan_GKE_TagsFromResourceLabels(t *testing.T) {
	fake := newFakeGCP()
	fake.GKEClusters = []*container.Cluster{
		gkeCluster("tagged", "1.29.4-gke.1043000", "us-central1", "RUNNING", true, map[string]string{
			"env":  "prod",
			"team": "platform",
		}),
	}
	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1)
	assert.Equal(t, map[string]string{"env": "prod", "team": "platform"}, res.Clusters[0].Tags)
}

func TestScan_ComputeCloudSQLAndGKE_AllThreeWalked(t *testing.T) {
	fake := newFakeGCP()
	fake.Zones = []*compute.Zone{
		{Name: "us-central1-a"},
		{Name: "us-west1-a"},
	}
	fake.InstancesByZone["us-central1-a"] = []*compute.Instance{
		{Name: "vm-1", MachineType: "zones/us-central1-a/machineTypes/e2-medium"},
		{Name: "vm-2", MachineType: "zones/us-central1-a/machineTypes/e2-medium", Labels: map[string]string{"otel": "v1"}},
	}
	fake.InstancesByZone["us-west1-a"] = []*compute.Instance{
		{Name: "vm-3", MachineType: "zones/us-west1-a/machineTypes/e2-medium"},
	}
	fake.CloudSQLInstances = []*sqladmin.DatabaseInstance{
		cloudSQLInstance("db-1", "POSTGRES_15", "us-central1", "db-custom-1-3840", true),
		cloudSQLInstance("db-2", "MYSQL_8_0", "us-west1", "db-n1-standard-2", false),
	}
	fake.GKEClusters = []*container.Cluster{
		gkeCluster("k8s-1", "1.29.4-gke.1043000", "us-central1", "RUNNING", true, nil),
		gkeCluster("k8s-2", "1.30.1-gke.500", "us-west1", "RUNNING", false, nil),
	}

	s := newScannerWithFake(t, fake, "p", "")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.Len(t, res.Compute, 3, "compute walk produced its rows")
	assert.Len(t, res.Databases, 2, "cloud sql walk produced its rows")
	assert.Len(t, res.Clusters, 2, "gke walk produced its rows")
	assert.False(t, res.Partial)
	// Tally:
	//   compute (1 otel of 3) + databases (1 query insights of 2) +
	//   clusters (1 mp enabled of 2) = 3 instrumented; 4 uninstrumented.
	assert.Equal(t, 3, res.InstrumentedCount)
	assert.Equal(t, 4, res.UninstrumentedCount)
}

func TestScan_GKE_RegionFilter(t *testing.T) {
	// Confirms the s.Region client-side filter applies to GKE the same
	// way it does to Cloud SQL (the API has no server-side region
	// filter on the "-" location wildcard; the walk reads every
	// cluster and the projection drops the ones outside the requested
	// region).
	fake := newFakeGCP()
	fake.GKEClusters = []*container.Cluster{
		gkeCluster("central-1", "1.29.4-gke.1043000", "us-central1", "RUNNING", true, nil),
		gkeCluster("central-2", "1.29.4-gke.1043000", "us-central1", "RUNNING", false, nil),
		gkeCluster("east-1", "1.30.1-gke.500", "us-east1", "RUNNING", true, nil),
		gkeCluster("west-1", "1.30.1-gke.500", "us-west1", "RUNNING", true, nil),
	}
	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 2)
	names := map[string]struct{}{}
	for _, c := range res.Clusters {
		names[c.Name] = struct{}{}
		assert.Equal(t, "us-central1", c.Region)
	}
	assert.Contains(t, names, "central-1")
	assert.Contains(t, names, "central-2")
	assert.NotContains(t, names, "east-1")
	assert.NotContains(t, names, "west-1")
}

func TestExtractMajorMinor_Standalone(t *testing.T) {
	// Direct exercise of the helper; the table test above covers it
	// via Scan, but the standalone form helps a debugger pinpoint a
	// regression to the helper rather than the walker.
	assert.Equal(t, "1.29", extractMajorMinor("1.29.4-gke.1043000"))
	assert.Equal(t, "1.30", extractMajorMinor("1.30.1-gke.500"))
	assert.Equal(t, "1.28", extractMajorMinor("1.28"))
	assert.Equal(t, "", extractMajorMinor(""))
}

// --- Serverless dispatcher tests --------------------------------------
//
// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) — the
// ScanServerless dispatcher fans to both Cloud Run + Cloud Functions
// in sequence. The two walks populate result.Serverless from disjoint
// surfaces ("cloudrun" vs "cloudfunc"); a failure on one surface
// must NOT contaminate the other.

// TestScanServerless_DispatchesToBothCloudRunAndCloudFunctions
// confirms the dispatcher fans to both walks in a single Scan call
// when both surfaces have inventory. The result must carry one row
// per surface row, each stamped with its own Surface discriminator.
func TestScanServerless_DispatchesToBothCloudRunAndCloudFunctions(t *testing.T) {
	fake := newFakeGCP()
	// Cloud Run seed: one service with the trace annotation.
	fake.CloudRunServicesByRegion["us-central1"] = []*run.Service{
		makeCloudRunService("api", "us-central1",
			map[string]string{CloudRunTraceAnnotation: "on"}, nil),
	}
	// Cloud Functions seed: one function with the OTel layer env.
	fake.CloudFunctions = []*cloudfunctions.CloudFunction{
		makeCloudFunction("etl", "us-central1", "python311",
			map[string]string{CloudFunctionsOTelLayerEnv: "true"}),
	}

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	cloudRun := serverlessBySurface(res.Serverless, cloudRunServerlessSurface)
	cloudFunc := serverlessBySurface(res.Serverless, cloudFuncServerlessSurface)
	require.Len(t, cloudRun, 1, "exactly one Cloud Run row expected")
	require.Len(t, cloudFunc, 1, "exactly one Cloud Functions row expected")

	assert.Equal(t, "api", cloudRun[0].ResourceName)
	assert.True(t, cloudRun[0].HasTraceAxis)

	assert.Equal(t, "etl", cloudFunc[0].ResourceName)
	assert.True(t, cloudFunc[0].HasOTelDistro)

	// Both rows count as instrumented under the OR rule, contributing
	// to the Result-level instrumented tally.
	assert.Equal(t, 2, res.InstrumentedCount)
	assert.False(t, res.Partial)
}

// TestScanServerless_PartialFailureOnCloudRunStillReturnsCloudFunc
// confirms the two walks are independent — a Cloud Run failure
// records the partial-failure entry but does NOT short-circuit the
// Cloud Functions walk. The operator still sees the Cloud Functions
// rows.
func TestScanServerless_PartialFailureOnCloudRunStillReturnsCloudFunc(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudRunListStatus = http.StatusForbidden
	fake.CloudFunctions = []*cloudfunctions.CloudFunction{
		makeCloudFunction("etl", "us-central1", "python311",
			map[string]string{CloudFunctionsOTelLayerEnv: "true"}),
		makeCloudFunction("workers", "us-central1", "nodejs20",
			map[string]string{GoogleCloudTraceEnv: "true"}),
	}

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "cloudrun 403 is partial, not hard")

	// Cloud Run failure recorded.
	assert.True(t, res.Partial)
	assert.Contains(t, res.FailedServices, ServiceIDCloudRun)
	// Cloud Functions surface NOT in the failed list.
	assert.NotContains(t, res.FailedServices, ServiceIDCloudFunctions)

	// Cloud Functions rows survived.
	cloudFunc := serverlessBySurface(res.Serverless, cloudFuncServerlessSurface)
	require.Len(t, cloudFunc, 2)
	// Cloud Run rows: none survived (the list call failed before
	// any service was projected).
	cloudRun := serverlessBySurface(res.Serverless, cloudRunServerlessSurface)
	assert.Empty(t, cloudRun)
}

// TestScanServerless_PartialFailureOnCloudFuncStillReturnsCloudRun —
// the symmetric inverse: a Cloud Functions failure records the
// partial-failure entry but the Cloud Run walk still produces its
// rows. Pins the "no contamination" invariant the dispatcher
// commits to.
func TestScanServerless_PartialFailureOnCloudFuncStillReturnsCloudRun(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudFunctionsListStatus = http.StatusForbidden
	fake.CloudRunServicesByRegion["us-central1"] = []*run.Service{
		makeCloudRunService("api", "us-central1",
			map[string]string{CloudRunTraceAnnotation: "on"}, nil),
	}

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.True(t, res.Partial)
	assert.Contains(t, res.FailedServices, ServiceIDCloudFunctions)
	assert.NotContains(t, res.FailedServices, ServiceIDCloudRun)

	cloudRun := serverlessBySurface(res.Serverless, cloudRunServerlessSurface)
	require.Len(t, cloudRun, 1)
	assert.True(t, cloudRun[0].HasTraceAxis)
}

// TestScanServerless_LocationEnumerationWhenRegionEmpty — when
// s.Region is empty the Cloud Run walk calls Locations.List first,
// then fans Services.List per location. The Cloud Functions walk
// uses the "-" wildcard so no separate location enumeration runs
// for that surface. Pins the location-discovery posture documented
// in cloudrun.go::cloudRunRegions godoc.
func TestScanServerless_LocationEnumerationWhenRegionEmpty(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudRunLocations = []*run.Location{
		{LocationId: "us-central1"},
		{LocationId: "europe-west1"},
	}
	fake.CloudRunServicesByRegion["us-central1"] = []*run.Service{
		makeCloudRunService("svc-a", "us-central1", nil, nil),
	}
	fake.CloudRunServicesByRegion["europe-west1"] = []*run.Service{
		makeCloudRunService("svc-b", "europe-west1", nil, nil),
	}

	s := newScannerWithFake(t, fake, "p", "" /* no region pin */)
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, fake.CloudRunLocationsCalls)
	assert.Equal(t, 1, fake.CloudRunListCalls["us-central1"])
	assert.Equal(t, 1, fake.CloudRunListCalls["europe-west1"])

	cloudRun := serverlessBySurface(res.Serverless, cloudRunServerlessSurface)
	require.Len(t, cloudRun, 2)
}

// TestScan_GCSObjectStores covers the object-store tier (coverage-parity
// arc slice 1): GCS buckets project into ObjectStoreSnapshot, and a
// bucket with usage/access logging configured counts as instrumented.
func TestScan_GCSObjectStores(t *testing.T) {
	fake := newFakeGCP()
	fake.Buckets = []*storage.Bucket{
		{
			Name:     "logs-enabled-bucket",
			Location: "US",
			Logging:  &storage.BucketLogging{LogBucket: "target-bucket"},
			Labels:   map[string]string{"env": "prod"},
		},
		{
			Name:     "no-logs-bucket",
			Location: "US-EAST1",
		},
	}
	s := newScannerWithFake(t, fake, "proj", "")
	result, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, result.ObjectStores, 2)

	var sawEnabled, sawDisabled bool
	for _, o := range result.ObjectStores {
		switch o.ResourceID {
		case "logs-enabled-bucket":
			sawEnabled = true
			assert.True(t, o.ServerAccessLoggingEnabled, "logging bucket should be instrumented")
			assert.Equal(t, "us", o.Region)
			assert.Equal(t, "prod", o.Tags["env"])
		case "no-logs-bucket":
			sawDisabled = true
			assert.False(t, o.ServerAccessLoggingEnabled)
		}
	}
	assert.True(t, sawEnabled && sawDisabled, "both buckets should be present")
	assert.GreaterOrEqual(t, result.InstrumentedCount, 1, "the logging bucket adds to instrumented count")
}

// TestScan_GCSListError_PartialNotFatal confirms a buckets.list failure
// is recorded as a partial failure (gcs) without failing the scan.
func TestScan_GCSListError_PartialNotFatal(t *testing.T) {
	fake := newFakeGCP()
	fake.BucketsListStatus = http.StatusForbidden
	s := newScannerWithFake(t, fake, "proj", "")
	result, err := s.Scan(context.Background())
	require.NoError(t, err)
	assert.True(t, result.Partial)
	found := false
	for _, fs := range result.FailedServices {
		if fs == ServiceIDGCS {
			found = true
		}
	}
	assert.True(t, found, "gcs should be in FailedServices: %v", result.FailedServices)
}
