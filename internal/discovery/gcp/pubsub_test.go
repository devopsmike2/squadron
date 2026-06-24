// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Event source tier slice 1 chunk 2 (v0.89.101, #735 Stream 133) GCP
// Pub/Sub scanner tests. Pins design doc §11 acceptance tests 4-6
// plus the absent-tracingConfig §12 ambiguity case, per-axis
// decoupling tests, and pagination/empty-response posture tests.
// Self-contained httptest fake serves GET /v1/projects/{project}/topics.

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

// fakePubSub is an httptest-backed mock of the GCP Pub/Sub REST API
// implementing the topics.list endpoint the chunk-2 walk exercises.
type fakePubSub struct {
	mu sync.Mutex
	// Topics: single-page list when Pages is empty.
	Topics []*pubsubTopic
	// Pages: multi-page sequence, indexed by inbound pageToken.
	Pages [][]*pubsubTopic
	// ListStatus: non-zero forces an error response.
	ListStatus int
	// ListCalls counts inbound list calls; pagination test asserts.
	ListCalls int
}

func newFakePubSub() *fakePubSub {
	return &fakePubSub{}
}

func (f *fakePubSub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		// Pub/Sub API path shape (REST, v1):
		//   /v1/projects/{project}/topics
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/topics") {
			f.ListCalls++
			if f.ListStatus != 0 {
				writeAPIError(w, f.ListStatus,
					statusReason(f.ListStatus), statusName(f.ListStatus))
				return
			}
			items, nextToken := f.topicsPage(r.URL.Query().Get("pageToken"))
			resp := pubsubListTopicsResponse{
				Topics:        items,
				NextPageToken: nextToken,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Unmatched path — surface as 404 so test failures are obvious.
		writeAPIError(w, http.StatusNotFound,
			fmt.Sprintf("unhandled mock path: %s", path), "NOT_FOUND")
	})
}

// topicsPage resolves the inbound pageToken into (items, nextToken).
// Token shape "page-N" (1-indexed); empty token selects page 0 (or
// the flat Topics list when no pages are configured).
func (f *fakePubSub) topicsPage(pageToken string) ([]*pubsubTopic, string) {
	if len(f.Pages) == 0 {
		return f.Topics, ""
	}
	idx := 0
	if pageToken != "" {
		var n int
		_, _ = fmt.Sscanf(pageToken, "page-%d", &n)
		idx = n
	}
	if idx >= len(f.Pages) {
		return nil, ""
	}
	items := f.Pages[idx]
	if idx+1 < len(f.Pages) {
		return items, fmt.Sprintf("page-%d", idx+1)
	}
	return items, ""
}

// newScannerWithPubSubFake wires a Scanner against the fake's
// httptest server. Mirrors newScannerWithWorkflowsFake.
func newScannerWithPubSubFake(t *testing.T, fake *fakePubSub, projectID string) *Scanner {
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

// makePubSubTopic — fixture builder. samplingRatio < 0 is the
// sentinel for "no tracingConfig" (absent-field test case); the
// builder leaves TracingConfig nil. Name uses the canonical
// "projects/{p}/topics/{n}" shape.
func makePubSubTopic(project, name string, samplingRatio float64) *pubsubTopic {
	t := &pubsubTopic{
		Name: fmt.Sprintf("projects/%s/topics/%s", project, name),
	}
	if samplingRatio >= 0 {
		t.TracingConfig = &pubsubTracingConfig{SamplingRatio: samplingRatio}
	}
	return t
}

// runPubSubScan wires a fake into a fresh Scanner and calls
// ScanPubSubTopics. Shared harness for the per-axis tests.
func runPubSubScan(t *testing.T, fake *fakePubSub, project string) []scanner.EventSourceInstanceSnapshot {
	t.Helper()
	s := newScannerWithPubSubFake(t, fake, project)
	out, err := s.ScanPubSubTopics(context.Background(), scanner.ScanScope{
		AccountID: project,
	})
	require.NoError(t, err)
	return out
}

// --- Acceptance tests -------------------------------------------------

// TestPubSubScanner_TopicWithSamplingRatioOne_HasTraceAxis — slice 1
// acceptance test 4: a topic with tracingConfig.samplingRatio=1.0
// flips HasTraceAxis. Pins the design doc §3.2 detection rule at
// the canonical "fully sampled" end of the [0, 1.0] range.
func TestPubSubScanner_TopicWithSamplingRatioOne_HasTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		name    = "orders"
	)
	fake := newFakePubSub()
	fake.Topics = []*pubsubTopic{
		makePubSubTopic(project, name, 1.0),
	}
	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 1)
	snap := out[0]
	assert.Equal(t, string(credstore.ProviderGCP), snap.Provider)
	assert.Equal(t, PubSubEventSourceSurface, snap.Surface)
	assert.Equal(t, project, snap.AccountID)
	assert.Equal(t, PubSubRegionGlobal, snap.Region)
	assert.Equal(t, PubSubSourceTypeTopic, snap.SourceType)
	assert.Equal(t, name, snap.ResourceName)
	assert.Equal(t,
		fmt.Sprintf("projects/%s/topics/%s", project, name),
		snap.ResourceARN)
	assert.True(t, snap.HasTraceAxis,
		"samplingRatio=1.0 must flip the trace axis")
	assert.False(t, snap.HasLogAxis,
		"slice 1 chunk 2 leaves HasLogAxis false on every Pub/Sub row")
	assert.True(t, snap.IsInstrumented(),
		"either-axis OR-rule: HasTraceAxis alone satisfies IsInstrumented")
	assert.Equal(t, 1.0, snap.Detail["tracing_sampling_ratio"],
		"raw samplingRatio surfaces in the Detail bag for the Inventory drilldown")
}

// TestPubSubScanner_TopicWithSamplingRatioZero_NoTraceAxis — slice 1
// acceptance test 5: a topic with tracingConfig.samplingRatio=0
// leaves HasTraceAxis false. Pins the negative case of the design
// doc §3.2 detection rule.
func TestPubSubScanner_TopicWithSamplingRatioZero_NoTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		name    = "audit"
	)
	fake := newFakePubSub()
	fake.Topics = []*pubsubTopic{
		makePubSubTopic(project, name, 0.0),
	}
	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"samplingRatio=0 must NOT flip the trace axis")
	assert.False(t, out[0].HasLogAxis)
	assert.False(t, out[0].IsInstrumented(),
		"both axes false → the topic is not instrumented")
	// The raw samplingRatio still surfaces in the Detail bag — the
	// Detail layer is free to distinguish "explicit zero" from
	// "absent" even though the HasTraceAxis predicate collapses them
	// per design doc §12.
	assert.Equal(t, 0.0, out[0].Detail["tracing_sampling_ratio"])
}

// TestPubSubScanner_TopicWithSamplingRatioPointFive_HasTraceAxis — any
// value strictly greater than 0 satisfies the slice-1 detection
// rule. Pins the threshold semantics: PubSubSamplingRatioMin is 0.0
// and the comparison is strict (>) not inclusive (>=).
func TestPubSubScanner_TopicWithSamplingRatioPointFive_HasTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		name    = "events"
	)
	fake := newFakePubSub()
	fake.Topics = []*pubsubTopic{
		makePubSubTopic(project, name, 0.5),
	}
	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasTraceAxis,
		"any samplingRatio > PubSubSamplingRatioMin (0.0) satisfies HasTraceAxis — 0.5 qualifies")
	assert.True(t, out[0].IsInstrumented())
	assert.Equal(t, 0.5, out[0].Detail["tracing_sampling_ratio"])
}

// TestPubSubScanner_TopicWithoutTracingConfig_NoTraceAxis — the
// §12 ambiguity case: a topic with no tracingConfig field on the
// wire (the field is omitted) parses to a nil TracingConfig
// pointer; the scanner treats this as samplingRatio=0 and leaves
// HasTraceAxis false. Confirms the Detail bag does NOT carry a
// tracing_sampling_ratio entry in this case (preserving the
// "absent vs. zero" distinction at the Detail layer even though
// HasTraceAxis collapses them).
func TestPubSubScanner_TopicWithoutTracingConfig_NoTraceAxis(t *testing.T) {
	const (
		project = "test-project"
		name    = "legacy"
	)
	fake := newFakePubSub()
	// samplingRatio = -1 sentinel → makePubSubTopic skips setting
	// TracingConfig, mirroring the API wire format that omits the
	// field entirely on a topic that was never configured for
	// tracing.
	fake.Topics = []*pubsubTopic{
		makePubSubTopic(project, name, -1),
	}
	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis,
		"absent tracingConfig must be treated as samplingRatio=0 → HasTraceAxis=false")
	assert.False(t, out[0].IsInstrumented())
	if out[0].Detail != nil {
		_, present := out[0].Detail["tracing_sampling_ratio"]
		assert.False(t, present,
			"Detail bag must NOT carry tracing_sampling_ratio when the wire field is absent — preserves §12 distinction at the drilldown layer")
	}
}

// TestPubSubScanner_TopicWithSchemaSettings_DetailRecordsSchema —
// slice 1 acceptance test 6: a topic with schemaSettings.schema set
// records the schema reference in the Detail bag. Pins design doc
// §3.2's pubsub-schema-attach detection input; the Detail entry is
// what the proposer's reasoning + the Inventory tab's drilldown
// both read.
func TestPubSubScanner_TopicWithSchemaSettings_DetailRecordsSchema(t *testing.T) {
	const (
		project = "test-project"
		name    = "shipments"
		schema  = "projects/test-project/schemas/shipment-v1"
	)
	fake := newFakePubSub()
	topic := makePubSubTopic(project, name, 1.0)
	topic.SchemaSettings = &pubsubSchemaSettings{
		Schema:   schema,
		Encoding: "JSON",
	}
	fake.Topics = []*pubsubTopic{topic}

	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Detail)
	assert.Equal(t, schema, out[0].Detail["schema"],
		"schemaSettings.schema must record in the Detail bag")
	assert.Equal(t, "JSON", out[0].Detail["schema_encoding"],
		"schemaSettings.encoding surfaces alongside schema when present")
	// Slice 1 chunk 2: schema attach is NOT a HasLogAxis signal —
	// schema is a payload-shape contract, not a logging-destination
	// signal. The HasTraceAxis flip here is driven by the
	// samplingRatio=1.0 input, not the schema.
	assert.False(t, out[0].HasLogAxis,
		"schemaSettings does NOT flip HasLogAxis in slice 1 chunk 2")
}

// TestPubSubScanner_TopicWithMessageStoragePolicy_DetailRecordsRegions
// — a topic with messageStoragePolicy.allowedPersistenceRegions
// populated records the region list in the Detail bag. Per design
// doc §3.2 + §11, the storage policy is informational only (no
// recommendation kind) but the Detail entry surfaces it for the
// per-cloud Inventory tab's drilldown so the operator sees the
// region constraint alongside the trace/log axes.
func TestPubSubScanner_TopicWithMessageStoragePolicy_DetailRecordsRegions(t *testing.T) {
	const (
		project = "test-project"
		name    = "eu-only"
	)
	fake := newFakePubSub()
	topic := makePubSubTopic(project, name, 0.0)
	topic.MessageStoragePolicy = &pubsubMessageStoragePolicy{
		AllowedPersistenceRegions: []string{"europe-west1", "europe-west4"},
	}
	fake.Topics = []*pubsubTopic{topic}

	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Detail)
	regions, ok := out[0].Detail["allowed_persistence_regions"].([]string)
	require.True(t, ok,
		"allowed_persistence_regions must be a []string entry in the Detail bag")
	assert.Equal(t,
		[]string{"europe-west1", "europe-west4"},
		regions,
		"region list must surface verbatim (order preserved)")
	// Informational only — neither axis flips off the storage policy.
	assert.False(t, out[0].HasTraceAxis)
	assert.False(t, out[0].HasLogAxis)
}

// TestPubSubScanner_PaginationFollowsNextPageToken — two list pages
// surface three total topics. The scanner must follow nextPageToken
// until the response carries an empty token. Mirrors the Workflows
// scanner's TestWorkflowsScanner_PaginationFollowsNextPageToken
// contract.
func TestPubSubScanner_PaginationFollowsNextPageToken(t *testing.T) {
	const project = "test-project"
	fake := newFakePubSub()
	fake.Pages = [][]*pubsubTopic{
		{
			makePubSubTopic(project, "alpha", 1.0),
			makePubSubTopic(project, "beta", 0.0),
		},
		{
			makePubSubTopic(project, "gamma", 0.5),
		},
	}
	out := runPubSubScan(t, fake, project)
	require.Len(t, out, 3,
		"both list pages must surface — three topics total")
	names := map[string]bool{}
	for _, snap := range out {
		names[snap.ResourceName] = true
	}
	assert.True(t, names["alpha"])
	assert.True(t, names["beta"])
	assert.True(t, names["gamma"])
	assert.Equal(t, 2, fake.ListCalls,
		"the pagination loop must issue exactly one call per page (2 pages → 2 calls)")
}

// TestPubSubScanner_EmptyResponseReturnsEmptySlice — zero topics in
// the project surface as an empty result without error. Pins the
// "operator has not yet created any topics" posture: not an error,
// just an empty Inventory tab.
func TestPubSubScanner_EmptyResponseReturnsEmptySlice(t *testing.T) {
	fake := newFakePubSub()
	// Topics seeded empty by default — the fake returns an empty
	// slice with no nextPageToken.
	out := runPubSubScan(t, fake, "test-project")
	assert.Len(t, out, 0)
	assert.Equal(t, 1, fake.ListCalls,
		"the walk must still issue a single list call even when the result is empty")
}

// TestPubSubScanner_ScanEventSourcesDelegatesToScanPubSubTopics —
// the chunk-5 EventSourceDiscoveryScanner interface dispatch (the
// handler-side runtime type assertion). Asserts that the event
// source entry point produces the same snapshots as the direct
// Pub/Sub walk so the handler-side dispatcher lands on the correct
// method.
func TestPubSubScanner_ScanEventSourcesDelegatesToScanPubSubTopics(t *testing.T) {
	const (
		project = "test-project"
		name    = "demo"
	)
	fake := newFakePubSub()
	fake.Topics = []*pubsubTopic{
		makePubSubTopic(project, name, 1.0),
	}
	s := newScannerWithPubSubFake(t, fake, project)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		AccountID: project,
	})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, PubSubEventSourceSurface, out[0].Surface)
	assert.Equal(t, string(credstore.ProviderGCP), out[0].Provider)
	assert.True(t, out[0].HasTraceAxis,
		"the entry-point delegation must produce the same axis outcomes as ScanPubSubTopics direct")
}
