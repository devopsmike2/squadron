// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Event source tier slice 5 chunk 1 (v0.89.144, #784 Stream 182) GCP
// Cloud Tasks scanner + ScanEventSources dispatcher extension tests.
// Pins design doc §11 acceptance tests 1-13. Self-contained
// httptest fake serves
// GET /v2/projects/{project}/locations/{location}/queues per region.

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

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// --- Test doubles -----------------------------------------------------

// fakeCloudTasks is an httptest-backed mock of the GCP Cloud Tasks
// REST API implementing the projects.locations.queues.list endpoint
// the chunk-1 walk exercises.
type fakeCloudTasks struct {
	mu sync.Mutex
	// QueuesByRegion: single-page list per region.
	QueuesByRegion map[string][]*cloudTasksQueue
	// PagesByRegion: multi-page sequence per region, indexed by inbound
	// pageToken (token "page-N" 1-indexed).
	PagesByRegion map[string][][]*cloudTasksQueue
	// ListStatusByRegion: non-zero per region forces an error response.
	ListStatusByRegion map[string]int
	// ListCalls per region.
	ListCallsByRegion map[string]int
	// AlsoServePubSubEmpty: when true, the fake responds to Pub/Sub
	// topics.list with an empty topic set so the dispatcher tests can
	// drive both surfaces against a single fake server.
	AlsoServePubSubEmpty bool
	// PubSubListStatus: when non-zero, fails the Pub/Sub topics.list call.
	PubSubListStatus int
}

func newFakeCloudTasks() *fakeCloudTasks {
	return &fakeCloudTasks{
		QueuesByRegion:     map[string][]*cloudTasksQueue{},
		PagesByRegion:      map[string][][]*cloudTasksQueue{},
		ListStatusByRegion: map[string]int{},
		ListCallsByRegion:  map[string]int{},
	}
}

func (f *fakeCloudTasks) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path

		// Cloud Tasks API path shape:
		//   /v2/projects/{project}/locations/{location}/queues
		if strings.HasPrefix(path, "/v2/projects/") && strings.HasSuffix(path, "/queues") {
			region := parseLocationFromQueuesPath(path)
			f.ListCallsByRegion[region]++
			if code, ok := f.ListStatusByRegion[region]; ok && code != 0 {
				writeAPIError(w, code, statusReason(code), statusName(code))
				return
			}
			items, nextToken := f.queuesPage(region, r.URL.Query().Get("pageToken"))
			resp := cloudTasksListQueuesResponse{
				Queues:        items,
				NextPageToken: nextToken,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Pub/Sub topics.list: only honored when AlsoServePubSubEmpty is
		// true (dispatcher tests). Returns empty by default.
		if f.AlsoServePubSubEmpty && strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/topics") {
			if f.PubSubListStatus != 0 {
				writeAPIError(w, f.PubSubListStatus, statusReason(f.PubSubListStatus), statusName(f.PubSubListStatus))
				return
			}
			resp := pubsubListTopicsResponse{}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Pub/Sub subscriptions.list (slice 2 chunk 2 propagation
		// walk): respond with an empty list so the topic walk doesn't
		// 404 when the dispatcher tests use AlsoServePubSubEmpty.
		if f.AlsoServePubSubEmpty && strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/subscriptions") {
			resp := pubsubListSubscriptionsResponse{}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		writeAPIError(w, http.StatusNotFound,
			fmt.Sprintf("unhandled mock path: %s", path), "NOT_FOUND")
	})
}

// queuesPage resolves the inbound pageToken into (items, nextToken).
// Token shape "page-N" (1-indexed); empty token selects page 0 (or
// the flat QueuesByRegion list when no pages are configured).
func (f *fakeCloudTasks) queuesPage(region, pageToken string) ([]*cloudTasksQueue, string) {
	pages, hasPages := f.PagesByRegion[region]
	if !hasPages || len(pages) == 0 {
		return f.QueuesByRegion[region], ""
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

// parseLocationFromQueuesPath extracts the {location} segment from a
// "/v2/projects/{p}/locations/{location}/queues" path.
func parseLocationFromQueuesPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// newScannerWithCloudTasksFake wires a Scanner against the fake's
// httptest server. Mirrors newScannerWithPubSubFake.
func newScannerWithCloudTasksFake(t *testing.T, fake *fakeCloudTasks, projectID string) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		ProjectID:  projectID,
		SAJSON:     nil,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
}

// makeCloudTasksQueue — fixture builder. Returns a queue with the
// canonical "projects/{p}/locations/{r}/queues/{n}" shape.
func makeCloudTasksQueue(project, region, name string) *cloudTasksQueue {
	return &cloudTasksQueue{
		Name: fmt.Sprintf("projects/%s/locations/%s/queues/%s", project, region, name),
	}
}

// runCloudTasksScan wires a fake into a fresh Scanner and calls
// ScanCloudTasksQueues against the supplied region. Shared harness for
// the per-axis tests.
func runCloudTasksScan(t *testing.T, fake *fakeCloudTasks, project, region string) []scanner.EventSourceInstanceSnapshot {
	t.Helper()
	s := newScannerWithCloudTasksFake(t, fake, project)
	out, err := s.ScanCloudTasksQueues(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{region},
	})
	require.NoError(t, err)
	return out
}

// --- Acceptance tests -------------------------------------------------

// TestScanCloudTasksQueues_ListReturnsQueues_Paginated — slice 5
// acceptance test 1: a paginated list response across a location is
// walked end-to-end. Three queues across two pages surface as three
// snapshots.
func TestScanCloudTasksQueues_ListReturnsQueues_Paginated(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	fake.PagesByRegion[region] = [][]*cloudTasksQueue{
		{
			makeCloudTasksQueue(project, region, "alpha"),
			makeCloudTasksQueue(project, region, "beta"),
		},
		{
			makeCloudTasksQueue(project, region, "gamma"),
		},
	}
	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 3,
		"both list pages must surface — three queues total")
	names := map[string]bool{}
	for _, snap := range out {
		names[snap.ResourceName] = true
	}
	assert.True(t, names["alpha"])
	assert.True(t, names["beta"])
	assert.True(t, names["gamma"])
	assert.Equal(t, 2, fake.ListCallsByRegion[region],
		"the pagination loop must issue exactly one call per page (2 pages → 2 calls)")
}

// TestScanCloudTasksQueues_QueueWithMaxAttemptsGreaterThanZero_HasTraceAxis —
// slice 5 acceptance test 2: a queue with retryConfig.maxAttempts > 0
// flips HasTraceAxis. Pins design doc §3 axis 1.
func TestScanCloudTasksQueues_QueueWithMaxAttemptsGreaterThanZero_HasTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "with-retry")
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 5}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasTraceAxis,
		"maxAttempts > 0 must flip the trace axis per §3 axis 1")
	assert.False(t, out[0].HasLogAxis)
	assert.Equal(t, int32(5), out[0].Detail["max_attempts"],
		"raw max_attempts surfaces in the Detail bag")
}

// TestScanCloudTasksQueues_QueueWithMaxAttemptsMinusOne_HasTraceAxis —
// slice 5 acceptance test 3: a queue with the unlimited-retry sentinel
// maxAttempts = -1 flips HasTraceAxis. Load-bearing per design doc
// §3 ("treats both > 0 AND -1 as configured retry").
func TestScanCloudTasksQueues_QueueWithMaxAttemptsMinusOne_HasTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "unlimited-retry")
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: CloudTasksMaxAttemptsUnlimited}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasTraceAxis,
		"maxAttempts = -1 (unlimited) must flip the trace axis per §3 axis 1")
	assert.Equal(t, int32(CloudTasksMaxAttemptsUnlimited), out[0].Detail["max_attempts"])
}

// TestScanCloudTasksQueues_QueueWithMaxAttemptsZero_NoTraceAxis —
// slice 5 acceptance test 4: a queue with retryConfig.maxAttempts = 0
// leaves HasTraceAxis false. The canonical recommendation-trigger case
// per design doc §3.
func TestScanCloudTasksQueues_QueueWithMaxAttemptsZero_NoTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "no-retry")
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 0}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"maxAttempts = 0 must NOT flip the trace axis — fires cloudtasks-retry-policy-enable")
	assert.Equal(t, int32(0), out[0].Detail["max_attempts"],
		"explicit zero surfaces in the Detail bag — preserves absent-vs-zero distinction")
}

// TestScanCloudTasksQueues_QueueWithoutRetryConfig_NoTraceAxis — an
// absent retryConfig is the §3 ambiguity case: a queue with no
// retryConfig field is indistinguishable from one explicitly set to
// maxAttempts = 0, and both leave HasTraceAxis false. Confirms the
// Detail bag does NOT carry a max_attempts entry when the wire field
// is absent — preserving the §3 absent-vs-zero distinction at the
// drilldown layer.
func TestScanCloudTasksQueues_QueueWithoutRetryConfig_NoTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "legacy")
	// RetryConfig left nil deliberately.
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"absent retryConfig must leave the trace axis false")
	if out[0].Detail != nil {
		_, present := out[0].Detail["max_attempts"]
		assert.False(t, present,
			"Detail bag must NOT carry max_attempts when the wire field is absent")
	}
}

// TestScanCloudTasksQueues_QueueWithStackdriverSamplingRatioGreaterThanZero_HasLogAxis
// — slice 5 acceptance test 5: a queue with
// stackdriverLoggingConfig.samplingRatio > 0 flips HasLogAxis. Pins
// design doc §3 axis 2.
func TestScanCloudTasksQueues_QueueWithStackdriverSamplingRatioGreaterThanZero_HasLogAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "with-logging")
	q.StackdriverLoggingConfig = &cloudTasksStackdriverLoggingConfig{SamplingRatio: 1.0}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasLogAxis,
		"samplingRatio > 0 must flip the log axis per §3 axis 2")
	assert.False(t, out[0].HasTraceAxis)
	assert.Equal(t, 1.0, out[0].Detail["stackdriver_sampling_ratio"])
}

// TestScanCloudTasksQueues_QueueWithStackdriverSamplingRatioZero_NoLogAxis
// — slice 5 acceptance test 6: a queue with explicit
// samplingRatio = 0 leaves HasLogAxis false. The Detail bag still
// surfaces the explicit zero so the drilldown can distinguish
// absent-vs-zero.
func TestScanCloudTasksQueues_QueueWithStackdriverSamplingRatioZero_NoLogAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "zero-sampling")
	q.StackdriverLoggingConfig = &cloudTasksStackdriverLoggingConfig{SamplingRatio: 0}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis,
		"samplingRatio = 0 must NOT flip the log axis — fires cloudtasks-logging-enable")
	assert.Equal(t, 0.0, out[0].Detail["stackdriver_sampling_ratio"])
}

// TestScanCloudTasksQueues_QueueWithoutStackdriverLoggingConfig_NoLogAxis
// — slice 5 acceptance test 7: a queue with no
// stackdriverLoggingConfig at all leaves HasLogAxis false. The
// Detail bag does NOT carry a stackdriver_sampling_ratio entry —
// preserves absent-vs-zero distinction at the drilldown layer.
func TestScanCloudTasksQueues_QueueWithoutStackdriverLoggingConfig_NoLogAxis(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "no-logging")
	// StackdriverLoggingConfig left nil.
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis,
		"absent stackdriverLoggingConfig must leave the log axis false")
	if out[0].Detail != nil {
		_, present := out[0].Detail["stackdriver_sampling_ratio"]
		assert.False(t, present,
			"Detail bag must NOT carry stackdriver_sampling_ratio when the wire field is absent")
	}
}

// TestScanCloudTasksQueues_PausedState_DetailRecordsState — slice 5
// acceptance test 8: a queue in the PAUSED state records the state in
// the Detail bag. Informational only — does NOT flip an axis.
func TestScanCloudTasksQueues_PausedState_DetailRecordsState(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "paused-queue")
	q.State = "PAUSED"
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Detail)
	assert.Equal(t, "PAUSED", out[0].Detail["state"],
		"PAUSED state must record in the Detail bag for the Inventory drilldown")
}

// TestScanCloudTasksQueues_QueueWithPurgeTime_DetailRecordsPurgeTime —
// slice 5 acceptance test 9: a queue with purgeTime set records the
// purge timestamp in the Detail bag. Informational only.
func TestScanCloudTasksQueues_QueueWithPurgeTime_DetailRecordsPurgeTime(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	purge := "2024-06-01T12:34:56Z"
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "purged-queue")
	q.PurgeTime = &purge
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Detail)
	assert.Equal(t, purge, out[0].Detail["purge_time"])
}

// TestScanCloudTasksQueues_RateLimitsRecordedInDetail — rate limits
// surface in the Detail bag when positive. Informational only per
// design doc §3 — does NOT flip an axis.
func TestScanCloudTasksQueues_RateLimitsRecordedInDetail(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, "rate-limited")
	q.RateLimits = &cloudTasksRateLimits{
		MaxDispatchesPerSecond:  100.5,
		MaxConcurrentDispatches: 10,
		MaxBurstSize:            20,
	}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Detail)
	assert.Equal(t, 100.5, out[0].Detail["max_dispatches_per_second"])
	assert.Equal(t, int32(10), out[0].Detail["max_concurrent_dispatches"])
	assert.Equal(t, int32(20), out[0].Detail["max_burst_size"])
	assert.False(t, out[0].HasTraceAxis,
		"rate limits are informational only — do NOT flip the trace axis")
	assert.False(t, out[0].HasLogAxis,
		"rate limits are informational only — do NOT flip the log axis")
}

// TestScanCloudTasksQueues_PerRegionPartialFailureContinues — per-region
// failure is non-fatal: region A fails, region B succeeds, the scan
// returns region B's queues with no error. Mirrors the workflows.go
// per-region partial-failure posture.
func TestScanCloudTasksQueues_PerRegionPartialFailureContinues(t *testing.T) {
	const (
		project = "test-project"
		regionA = "us-central1"
		regionB = "us-east1"
	)
	fake := newFakeCloudTasks()
	fake.ListStatusByRegion[regionA] = http.StatusForbidden
	fake.QueuesByRegion[regionB] = []*cloudTasksQueue{
		makeCloudTasksQueue(project, regionB, "survivor"),
	}
	s := newScannerWithCloudTasksFake(t, fake, project)
	out, err := s.ScanCloudTasksQueues(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{regionA, regionB},
	})
	require.NoError(t, err,
		"per-region partial-failure posture: regionA failed but regionB succeeded → no error")
	require.Len(t, out, 1, "regionB's queue still surfaces")
	assert.Equal(t, "survivor", out[0].ResourceName)
	assert.Equal(t, regionB, out[0].Region)
}

// TestScanCloudTasksQueues_AllRegionsFail_ReturnsError — when every
// configured region fails, the walk returns the last error so the
// dispatcher's two-way posture records a partial failure against the
// cloudtasks surface.
func TestScanCloudTasksQueues_AllRegionsFail_ReturnsError(t *testing.T) {
	const (
		project = "test-project"
		regionA = "us-central1"
		regionB = "us-east1"
	)
	fake := newFakeCloudTasks()
	fake.ListStatusByRegion[regionA] = http.StatusForbidden
	fake.ListStatusByRegion[regionB] = http.StatusForbidden
	s := newScannerWithCloudTasksFake(t, fake, project)
	out, err := s.ScanCloudTasksQueues(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{regionA, regionB},
	})
	require.Error(t, err)
	assert.Empty(t, out)
	assert.Contains(t, err.Error(), ServiceIDCloudTasks)
}

// TestScanCloudTasksQueues_ResourceNamePopulatedFromResourceID — the
// trailing segment of the queue resource path projects into
// ResourceName; the full path projects into ResourceARN.
func TestScanCloudTasksQueues_ResourceNamePopulatedFromResourceID(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "payment-webhooks"
	)
	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{
		makeCloudTasksQueue(project, region, name),
	}
	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.Equal(t, name, out[0].ResourceName,
		"ResourceName must be the trailing segment of the resource path")
	assert.Equal(t,
		fmt.Sprintf("projects/%s/locations/%s/queues/%s", project, region, name),
		out[0].ResourceARN,
		"ResourceARN must be the full resource path verbatim")
}

// TestScanCloudTasksQueues_SurfaceIsCloudtasks — Provider/Surface/
// SourceType/AccountID/Region/IsInstrumented sanity check against a
// canonical queue.
func TestScanCloudTasksQueues_SurfaceIsCloudtasks(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		name    = "orders"
	)
	fake := newFakeCloudTasks()
	q := makeCloudTasksQueue(project, region, name)
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 3}
	q.StackdriverLoggingConfig = &cloudTasksStackdriverLoggingConfig{SamplingRatio: 0.5}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	snap := out[0]
	assert.Equal(t, string(credstore.ProviderGCP), snap.Provider)
	assert.Equal(t, CloudTasksSurface, snap.Surface)
	assert.Equal(t, "cloudtasks", snap.Surface,
		"CloudTasksSurface constant must equal the literal \"cloudtasks\"")
	assert.Equal(t, CloudTasksSourceTypeQueue, snap.SourceType)
	assert.Equal(t, "queue", snap.SourceType)
	assert.Equal(t, project, snap.AccountID)
	assert.Equal(t, region, snap.Region)
	assert.True(t, snap.HasTraceAxis)
	assert.True(t, snap.HasLogAxis)
	assert.True(t, snap.IsInstrumented())
}

// TestCloudTasksQueueNameFromResourceID_ExtractsLastSegment — pure-
// function unit test of the trailing-segment extraction helper.
func TestCloudTasksQueueNameFromResourceID_ExtractsLastSegment(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"projects/my-project/locations/us-central1/queues/my-queue", "my-queue"},
		{"projects/p/locations/r/queues/n", "n"},
		// Defensive: input without slashes returns verbatim.
		{"bare-name", "bare-name"},
		// Defensive: empty input returns empty.
		{"", ""},
	}
	for _, tc := range tests {
		got := cloudTasksQueueNameFromResourceID(tc.input)
		assert.Equal(t, tc.want, got, "input=%q", tc.input)
	}
}

// --- Two-way dispatcher tests ----------------------------------------
//
// ScanEventSources fans out across Pub/Sub + Cloud Tasks with a
// two-way partial-scan posture per design doc §5. Tests 10 / 11 / 12 /
// 13 of the slice 5 design doc pin the contract: both surfaces
// dispatched independently; either one failing does NOT block the
// other; only when BOTH fail does the dispatcher return a non-nil
// error wrapping every per-surface cause.

// newScannerWithDualFake wires a Scanner against a shared httptest
// server that serves BOTH Pub/Sub topics.list AND Cloud Tasks
// queues.list. The fake's AlsoServePubSubEmpty flag controls the
// Pub/Sub response (empty by default); PubSubListStatus drives a
// failure. Cloud Tasks paths use the existing per-region routing.
func newScannerWithDualFake(t *testing.T, fake *fakeCloudTasks, projectID string) *Scanner {
	t.Helper()
	fake.AlsoServePubSubEmpty = true
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		ProjectID:  projectID,
		SAJSON:     nil,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
}

// TestScanEventSources_DispatchesToBothPubSubAndCloudTasks — slice 5
// acceptance test 10: the two-way dispatcher returns BOTH Pub/Sub
// topics AND Cloud Tasks queues when both surfaces produce data.
func TestScanEventSources_DispatchesToBothPubSubAndCloudTasks(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		topic   = "orders"
		queue   = "webhook-delivery"
	)
	fake := newFakeCloudTasks()
	// Pub/Sub: AlsoServePubSubEmpty default is false until
	// newScannerWithDualFake flips it. We override with a topic.
	fake.AlsoServePubSubEmpty = true
	// Seed Cloud Tasks queues.
	q := makeCloudTasksQueue(project, region, queue)
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 5}
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}
	// Override the empty Pub/Sub path with a single-topic response via
	// the dispatcher-specific handler. The fake's handler always returns
	// empty for Pub/Sub when AlsoServePubSubEmpty is true; the
	// dispatcher test only needs to confirm BOTH surfaces dispatched,
	// not the Pub/Sub-topic-content path (covered by pubsub_test.go).
	// We add one topic via a custom server to assert both surfaces.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Pub/Sub topics.list — return one topic.
		if strings.HasSuffix(path, "/topics") && strings.HasPrefix(path, "/v1/") {
			resp := pubsubListTopicsResponse{
				Topics: []*pubsubTopic{
					{Name: fmt.Sprintf("projects/%s/topics/%s", project, topic)},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Pub/Sub subscriptions.list — empty.
		if strings.HasSuffix(path, "/subscriptions") && strings.HasPrefix(path, "/v1/") {
			resp := pubsubListSubscriptionsResponse{}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Cloud Tasks queues.list — return our seeded queue when region matches.
		if strings.HasPrefix(path, "/v2/projects/") && strings.HasSuffix(path, "/queues") {
			loc := parseLocationFromQueuesPath(path)
			if loc == region {
				resp := cloudTasksListQueuesResponse{Queues: []*cloudTasksQueue{q}}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			resp := cloudTasksListQueuesResponse{}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		writeAPIError(w, http.StatusNotFound, "unhandled: "+path, "NOT_FOUND")
	}))
	t.Cleanup(srv.Close)
	s := &Scanner{
		ProjectID:  project,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}

	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{region},
	})
	require.NoError(t, err)
	// Slice 10 (v0.89.159) extended the GCP dispatcher to three-way
	// (PubSub + CloudTasks + Pub/Sub Lite). The Pub/Sub Lite scanner's
	// admin endpoint is the same Pub/Sub topics.list shape, so the
	// embedded handler returns the same single topic to Pub/Sub Lite,
	// producing a third snapshot (surface=pubsublite). Pinning the
	// three-way shape avoids a future four-way regression slipping
	// through silently.
	require.Len(t, out, 3, "three-way dispatcher must return topic + queue + pubsublite")

	// Verify all three surfaces present. Order: Pub/Sub topics, Cloud
	// Tasks queues, Pub/Sub Lite topics (matches the dispatcher's
	// sequential invocation).
	surfaces := map[string]bool{}
	for _, snap := range out {
		surfaces[snap.Surface] = true
	}
	assert.True(t, surfaces[PubSubEventSourceSurface], "pubsub topic must surface")
	assert.True(t, surfaces[CloudTasksSurface], "cloudtasks queue must surface")
	assert.True(t, surfaces[PubSubLiteSurface], "pubsublite topic must surface")
}

// TestScanEventSources_PubSubFails_CloudTasksStillSurfaces — slice 5
// acceptance test 11: the partial-scan posture in the Pub/Sub-fails
// direction. The Cloud Tasks queue still surfaces; the dispatcher
// returns no error because at least one surface succeeded.
func TestScanEventSources_PubSubFails_CloudTasksStillSurfaces(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		queue   = "still-here"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Pub/Sub: fail with 403.
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/topics") {
			writeAPIError(w, http.StatusForbidden, "pubsub denied", "PERMISSION_DENIED")
			return
		}
		// Cloud Tasks: succeed with one queue.
		if strings.HasPrefix(path, "/v2/projects/") && strings.HasSuffix(path, "/queues") {
			loc := parseLocationFromQueuesPath(path)
			if loc == region {
				q := makeCloudTasksQueue(project, region, queue)
				q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 5}
				resp := cloudTasksListQueuesResponse{Queues: []*cloudTasksQueue{q}}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			resp := cloudTasksListQueuesResponse{}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		writeAPIError(w, http.StatusNotFound, "unhandled: "+path, "NOT_FOUND")
	}))
	t.Cleanup(srv.Close)
	s := &Scanner{
		ProjectID:  project,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{region},
	})
	require.NoError(t, err,
		"two-way partial-scan posture: only Pub/Sub failed → no error")
	require.Len(t, out, 1, "Cloud Tasks queue still surfaces")
	assert.Equal(t, CloudTasksSurface, out[0].Surface)
	assert.Equal(t, queue, out[0].ResourceName)
}

// TestScanEventSources_CloudTasksFails_PubSubStillSurfaces — slice 5
// acceptance test 12: the partial-scan posture in the Cloud-Tasks-
// fails direction. The Pub/Sub topic still surfaces; the dispatcher
// returns no error because at least one surface succeeded.
func TestScanEventSources_CloudTasksFails_PubSubStillSurfaces(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
		topic   = "still-here"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Pub/Sub topics.list — succeed with one topic.
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/topics") {
			resp := pubsubListTopicsResponse{
				Topics: []*pubsubTopic{
					{Name: fmt.Sprintf("projects/%s/topics/%s", project, topic)},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Pub/Sub subscriptions.list — empty.
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/subscriptions") {
			resp := pubsubListSubscriptionsResponse{}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Cloud Tasks: fail with 403.
		if strings.HasPrefix(path, "/v2/projects/") && strings.HasSuffix(path, "/queues") {
			writeAPIError(w, http.StatusForbidden, "cloudtasks denied", "PERMISSION_DENIED")
			return
		}
		writeAPIError(w, http.StatusNotFound, "unhandled: "+path, "NOT_FOUND")
	}))
	t.Cleanup(srv.Close)
	s := &Scanner{
		ProjectID:  project,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{region},
	})
	require.NoError(t, err,
		"two-way partial-scan posture: only Cloud Tasks failed → no error")
	require.Len(t, out, 1, "Pub/Sub topic still surfaces")
	assert.Equal(t, PubSubEventSourceSurface, out[0].Surface)
	assert.Equal(t, topic, out[0].ResourceName)
}

// TestScanEventSources_BothFailReturnsErrorMentioningBothSurfaces —
// slice 5 acceptance test 13: the dispatcher's only error-returning
// path. BOTH surfaces fail. The returned error must mention pubsub
// AND cloudtasks so the operator-facing error message captures the
// full failure envelope.
func TestScanEventSources_BothFailReturnsErrorMentioningBothSurfaces(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Pub/Sub: fail.
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/topics") {
			writeAPIError(w, http.StatusForbidden, "pubsub boom", "PERMISSION_DENIED")
			return
		}
		// Cloud Tasks: fail.
		if strings.HasPrefix(path, "/v2/projects/") && strings.HasSuffix(path, "/queues") {
			writeAPIError(w, http.StatusForbidden, "cloudtasks kaboom", "PERMISSION_DENIED")
			return
		}
		writeAPIError(w, http.StatusNotFound, "unhandled: "+path, "NOT_FOUND")
	}))
	t.Cleanup(srv.Close)
	s := &Scanner{
		ProjectID:  project,
		httpClient: srv.Client(),
		endpoint:   srv.URL,
	}
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		AccountID: project,
		Regions:   []string{region},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pubsub")
	assert.Contains(t, err.Error(), "cloudtasks")
	assert.Empty(t, out, "both surfaces failed; no rows surface")
}
