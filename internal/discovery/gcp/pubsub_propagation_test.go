// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Event source tier slice 2 chunk 2 (v0.89.106, #742 Stream 140) GCP
// Pub/Sub propagation scanner tests. Pins design doc §11 acceptance
// tests 7-10 plus the schema-cache reuse, multi-axis accumulation, and
// schema-fetch-failure non-fatal posture cases.
//
// Why this file is separate from pubsub_test.go: the slice-1 tests
// pinned the trace/log axis fixtures; the slice-2 tests pin the
// per-schema + per-subscription propagation surface. Keeping them in
// two files makes the pre-slice-2 / post-slice-2 diff obvious to
// reviewers and lets the chunk-2 commit land without rewriting the
// slice-1 fixture helpers. Mirrors the AWS slice-2 file layout
// (eventbridge_propagation_test.go).

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

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// --- Test doubles -----------------------------------------------------

// fakePubSubPropagation is an httptest-backed mock of the GCP Pub/Sub
// REST API implementing topics.list + subscriptions.list +
// schemas.get — the three endpoints the slice-2 chunk-2 walk
// exercises. fakePubSub (in pubsub_test.go) only implements
// topics.list and the chunk-2 tests need the additional endpoints
// without disturbing the slice-1 test surface, so a parallel fake
// lands here.
type fakePubSubPropagation struct {
	mu sync.Mutex
	// Topics: the topics.list response payload.
	Topics []*pubsubTopic
	// Subscriptions: the subscriptions.list response payload.
	Subscriptions []*pubsubSubscription
	// Schemas: per-schema-name response payload for schemas.get.
	Schemas map[string]*pubsubSchema
	// SchemaStatuses: per-schema-name forced HTTP status (e.g. 403)
	// for the schema-fetch-failure non-fatal posture test.
	SchemaStatuses map[string]int
	// SubscriptionListStatus: non-zero forces an error on the
	// subscriptions.list call. The walk treats this as non-fatal —
	// the topic axis stays preserved by design.
	SubscriptionListStatus int
	// Per-endpoint call counters for cache-reuse + non-fatal assertions.
	TopicListCalls  int
	SubListCalls    int
	SchemaGetCalls  map[string]int
}

func newFakePubSubPropagation() *fakePubSubPropagation {
	return &fakePubSubPropagation{
		Schemas:        map[string]*pubsubSchema{},
		SchemaStatuses: map[string]int{},
		SchemaGetCalls: map[string]int{},
	}
}

func (f *fakePubSubPropagation) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		// /v1/projects/{p}/topics — slice-1 list endpoint.
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/topics") {
			f.TopicListCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pubsubListTopicsResponse{Topics: f.Topics})
			return
		}
		// /v1/projects/{p}/subscriptions — slice-2 chunk-2 endpoint.
		if strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/subscriptions") {
			f.SubListCalls++
			if f.SubscriptionListStatus != 0 {
				writeAPIError(w, f.SubscriptionListStatus,
					statusReason(f.SubscriptionListStatus),
					statusName(f.SubscriptionListStatus))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pubsubListSubscriptionsResponse{
				Subscriptions: f.Subscriptions,
			})
			return
		}
		// /v1/projects/{p}/schemas/{s} — slice-2 chunk-2 endpoint.
		// The schema-fetch URL also carries ?view=FULL; r.URL.Path
		// strips the query so a HasPrefix("/v1/projects/") +
		// Contains("/schemas/") check identifies the call.
		if strings.HasPrefix(path, "/v1/projects/") && strings.Contains(path, "/schemas/") {
			// Path is "/v1/" + schemaName, so trim the "/v1/" prefix
			// to recover the canonical "projects/{p}/schemas/{s}" key.
			schemaName := strings.TrimPrefix(path, "/v1/")
			f.SchemaGetCalls[schemaName]++
			if status, ok := f.SchemaStatuses[schemaName]; ok && status != 0 {
				writeAPIError(w, status,
					statusReason(status), statusName(status))
				return
			}
			schema, ok := f.Schemas[schemaName]
			if !ok {
				writeAPIError(w, http.StatusNotFound,
					fmt.Sprintf("schema %s not found", schemaName), "NOT_FOUND")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema)
			return
		}
		writeAPIError(w, http.StatusNotFound,
			fmt.Sprintf("unhandled mock path: %s", path), "NOT_FOUND")
	})
}

// newScannerWithPubSubPropagationFake wires a Scanner against the
// chunk-2 fake's httptest server. Mirrors newScannerWithPubSubFake.
func newScannerWithPubSubPropagationFake(t *testing.T, fake *fakePubSubPropagation, projectID string) *Scanner {
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

// runPubSubPropagationScan exercises ScanPubSubTopics against the
// chunk-2 fake. Shared harness for the propagation tests.
func runPubSubPropagationScan(t *testing.T, fake *fakePubSubPropagation, project string) []scanner.EventSourceInstanceSnapshot {
	t.Helper()
	s := newScannerWithPubSubPropagationFake(t, fake, project)
	out, err := s.ScanPubSubTopics(context.Background(), scanner.ScanScope{
		AccountID: project,
	})
	require.NoError(t, err)
	return out
}

// makePropagationTopic builds a pubsubTopic with samplingRatio=1.0
// (HasTraceAxis=true) and an optional schema reference. samplingRatio
// is fixed here because the slice-2 tests only care about the
// propagation axis; the trace/log axes are exercised in pubsub_test.go.
func makePropagationTopic(project, name, schema string) *pubsubTopic {
	t := &pubsubTopic{
		Name:          fmt.Sprintf("projects/%s/topics/%s", project, name),
		TracingConfig: &pubsubTracingConfig{SamplingRatio: 1.0},
	}
	if schema != "" {
		t.SchemaSettings = &pubsubSchemaSettings{Schema: schema, Encoding: "JSON"}
	}
	return t
}

// makePushSubscription builds a push-mode subscription on the supplied
// topic with the supplied attribute filter map. Empty filter map → no
// attribute filter; a non-nil map populates pushConfig.attributes.
func makePushSubscription(project, name, topicName string, attrs map[string]string) *pubsubSubscription {
	return &pubsubSubscription{
		Name:  fmt.Sprintf("projects/%s/subscriptions/%s", project, name),
		Topic: fmt.Sprintf("projects/%s/topics/%s", project, topicName),
		PushConfig: &pubsubPushConfig{
			PushEndpoint: "https://example.test/push",
			Attributes:   attrs,
		},
	}
}

// --- Per-schema-helper tests ------------------------------------------

// TestSchemaIncludesTraceparentField_DirectMatch — design doc §3.2,
// acceptance test 8. A schema definition containing the literal
// "traceparent" field name returns (true, "traceparent").
func TestSchemaIncludesTraceparentField_DirectMatch(t *testing.T) {
	def := `{"type":"record","fields":[{"name":"traceparent","type":"string"}]}`
	included, field := schemaIncludesTraceparentField(def)
	assert.True(t, included, "literal traceparent must match")
	assert.Equal(t, "traceparent", field,
		"the matched pattern is reported verbatim for the proposer's reasoning text")
}

// TestSchemaIncludesTraceparentField_CaseInsensitive — the substring
// match is case-insensitive; "TRACEPARENT" in a schema definition
// must still match the lowercase pattern.
func TestSchemaIncludesTraceparentField_CaseInsensitive(t *testing.T) {
	def := `message Event { string TRACEPARENT = 1; }`
	included, field := schemaIncludesTraceparentField(def)
	assert.True(t, included, "uppercase TRACEPARENT must match the lowercased pattern")
	assert.Equal(t, "traceparent", field)
}

// TestSchemaIncludesTraceparentField_VariantField_trace_context —
// design doc §3.2, acceptance test 8 variant. A schema using the
// snake_case "trace_context" field name must match.
func TestSchemaIncludesTraceparentField_VariantField_trace_context(t *testing.T) {
	def := `message Event { string trace_context = 2; }`
	included, field := schemaIncludesTraceparentField(def)
	assert.True(t, included)
	assert.Equal(t, "trace_context", field,
		"trace_context pattern is reported when it's the first match")
}

// TestSchemaIncludesTraceparentField_NotIncluded — design doc §3.2,
// acceptance test 9. A schema definition with no traceparent variant
// returns (false, "").
func TestSchemaIncludesTraceparentField_NotIncluded(t *testing.T) {
	def := `{"type":"record","fields":[{"name":"order_id","type":"string"}]}`
	included, field := schemaIncludesTraceparentField(def)
	assert.False(t, included)
	assert.Empty(t, field, "unmatched schemas carry no field name")
}

// TestSchemaIncludesTraceparentField_EmptySchema_NotIncluded — the
// empty-string edge case. A schema with no Definition (never
// populated, or schemas.get returning a stub) must NOT count as
// included; the per-topic aggregation will record a "missing field"
// note.
func TestSchemaIncludesTraceparentField_EmptySchema_NotIncluded(t *testing.T) {
	included, field := schemaIncludesTraceparentField("")
	assert.False(t, included, "empty schema definition is vacuously not included")
	assert.Empty(t, field)
}

// --- Scanner-level tests ----------------------------------------------

// TestPubSubScanner_TopicNoSchema_PropagationPreserved — design doc
// §3.2, acceptance test 7. A topic with no schemaSettings has no
// schema enforcement; the publisher controls attribute presence and
// propagation is preserved by default.
func TestPubSubScanner_TopicNoSchema_PropagationPreserved(t *testing.T) {
	const (
		project = "test-project"
		topic   = "orders"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{makePropagationTopic(project, topic, "")}
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasPropagationConfig,
		"no schemaSettings → schema axis preserved; no subscriptions → subscription axis preserved")
	assert.Empty(t, out[0].PropagationNotes,
		"preserved topics carry no propagation notes")
	assert.Zero(t, fake.SchemaGetCalls["projects/test-project/schemas/x"],
		"no schemaSettings → no schemas.get call must be issued")
}

// TestPubSubScanner_TopicWithSchemaIncludingTraceparent_PropagationPreserved
// — design doc §3.2: a schema-attached topic whose schema includes
// the traceparent field preserves the propagation axis.
func TestPubSubScanner_TopicWithSchemaIncludingTraceparent_PropagationPreserved(t *testing.T) {
	const (
		project    = "test-project"
		topic      = "orders"
		schemaName = "projects/test-project/schemas/order-v1"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{makePropagationTopic(project, topic, schemaName)}
	fake.Schemas[schemaName] = &pubsubSchema{
		Name:       schemaName,
		Type:       "AVRO",
		Definition: `{"type":"record","fields":[{"name":"traceparent","type":"string"},{"name":"order_id","type":"string"}]}`,
	}
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasPropagationConfig,
		"schema includes traceparent → schema axis preserved")
	assert.Empty(t, out[0].PropagationNotes)
	assert.Equal(t, 1, fake.SchemaGetCalls[schemaName],
		"the schema must have been fetched exactly once")
}

// TestPubSubScanner_TopicWithSchemaMissingTraceparent_PropagationBroken
// — design doc §3.2, acceptance test 9. A schema-attached topic
// whose schema omits every traceparent variant breaks the
// propagation axis and the note names the schema reference for the
// proposer's reasoning text.
func TestPubSubScanner_TopicWithSchemaMissingTraceparent_PropagationBroken(t *testing.T) {
	const (
		project    = "test-project"
		topic      = "shipments"
		schemaName = "projects/test-project/schemas/shipment-v1"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{makePropagationTopic(project, topic, schemaName)}
	fake.Schemas[schemaName] = &pubsubSchema{
		Name:       schemaName,
		Type:       "AVRO",
		Definition: `{"type":"record","fields":[{"name":"shipment_id","type":"string"}]}`,
	}
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasPropagationConfig,
		"schema missing every traceparent variant → schema axis broken")
	require.NotEmpty(t, out[0].PropagationNotes)
	assert.Contains(t, out[0].PropagationNotes[0], schemaName,
		"the note quotes the schema reference so the proposer can name it")
	assert.Contains(t, out[0].PropagationNotes[0], "missing traceparent field")
}

// TestPubSubScanner_TopicWithSubscriptionAttrFilterExcludingTraceparent_PropagationBroken
// — design doc §3.2, acceptance test 10. A push-mode subscription
// whose pushConfig.attributes filter omits every traceparent variant
// breaks propagation for the parent topic.
func TestPubSubScanner_TopicWithSubscriptionAttrFilterExcludingTraceparent_PropagationBroken(t *testing.T) {
	const (
		project = "test-project"
		topic   = "events"
		sub     = "events-push"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{makePropagationTopic(project, topic, "")}
	fake.Subscriptions = []*pubsubSubscription{
		makePushSubscription(project, sub, topic, map[string]string{
			"order_id":    "value",
			"customer_id": "value",
		}),
	}
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasPropagationConfig,
		"push subscription with attribute filter omitting traceparent → subscription axis broken")
	require.NotEmpty(t, out[0].PropagationNotes)
	assert.Contains(t, out[0].PropagationNotes[0], sub,
		"the note quotes the subscription name for proposer reasoning")
	assert.Contains(t, out[0].PropagationNotes[0], "attribute filter excluding traceparent")
}

// TestPubSubScanner_SchemaCacheReusedAcrossTopics_FetchOnce — slice-2
// chunk-2 schema cache contract. Two topics referencing the same
// schema must produce exactly one schemas.get call (the cache
// amortizes the fetch).
func TestPubSubScanner_SchemaCacheReusedAcrossTopics_FetchOnce(t *testing.T) {
	const (
		project    = "test-project"
		schemaName = "projects/test-project/schemas/shared-v1"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{
		makePropagationTopic(project, "alpha", schemaName),
		makePropagationTopic(project, "beta", schemaName),
		makePropagationTopic(project, "gamma", schemaName),
	}
	fake.Schemas[schemaName] = &pubsubSchema{
		Name:       schemaName,
		Type:       "AVRO",
		Definition: `{"type":"record","fields":[{"name":"traceparent","type":"string"}]}`,
	}
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 3, "three topics must surface")
	for _, snap := range out {
		assert.True(t, snap.HasPropagationConfig,
			"shared schema includes traceparent → every topic preserved")
	}
	assert.Equal(t, 1, fake.SchemaGetCalls[schemaName],
		"the schema cache must fetch the shared schema exactly once across all topics")
}

// TestPubSubScanner_PropagationNotesAccumulateAcrossAxes — both the
// schema and the subscription axes fail on the same topic. The
// PropagationNotes slice carries one entry per failing axis so the
// proposer's chunk-5 reasoning text can walk the full list.
func TestPubSubScanner_PropagationNotesAccumulateAcrossAxes(t *testing.T) {
	const (
		project    = "test-project"
		topic      = "audit"
		schemaName = "projects/test-project/schemas/audit-v1"
		sub        = "audit-push"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{makePropagationTopic(project, topic, schemaName)}
	fake.Schemas[schemaName] = &pubsubSchema{
		Name:       schemaName,
		Type:       "AVRO",
		Definition: `{"type":"record","fields":[{"name":"event_type","type":"string"}]}`,
	}
	fake.Subscriptions = []*pubsubSubscription{
		makePushSubscription(project, sub, topic, map[string]string{
			"event_type": "value",
		}),
	}
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasPropagationConfig,
		"both axes fail → the topic axis is the AND, so false")
	require.GreaterOrEqual(t, len(out[0].PropagationNotes), 2,
		"each failing axis contributes its own note")
	joined := strings.Join(out[0].PropagationNotes, "\n")
	assert.Contains(t, joined, "missing traceparent field",
		"the schema-axis note is present")
	assert.Contains(t, joined, "attribute filter excluding traceparent",
		"the subscription-axis note is present")
}

// TestPubSubScanner_SchemaFetchFailureNonFatal — a 403 on schemas.get
// is recoverable: the topic still surfaces, the propagation axis
// stays true (Squadron declines to emit a false positive against a
// schema it couldn't read), and an informational note carries the
// fetch failure cause.
func TestPubSubScanner_SchemaFetchFailureNonFatal(t *testing.T) {
	const (
		project    = "test-project"
		topic      = "orders"
		schemaName = "projects/test-project/schemas/order-v1"
	)
	fake := newFakePubSubPropagation()
	fake.Topics = []*pubsubTopic{makePropagationTopic(project, topic, schemaName)}
	fake.SchemaStatuses[schemaName] = http.StatusForbidden
	out := runPubSubPropagationScan(t, fake, project)
	require.Len(t, out, 1, "the topic must still surface")
	assert.True(t, out[0].HasPropagationConfig,
		"a non-fatal schema fetch failure must NOT flip the axis to false")
	require.NotEmpty(t, out[0].PropagationNotes,
		"the failure surfaces as an informational note")
	assert.Contains(t, out[0].PropagationNotes[0], schemaName)
	assert.Contains(t, out[0].PropagationNotes[0], "fetch failed",
		"the note explains the cause for the operator")
}

// TestSubscriptionPreservesTraceparent_PullModeAlwaysPreserved — a
// pull-mode subscription (no pushConfig.pushEndpoint) always
// preserves propagation; the consumer SDK reads message attributes
// verbatim with no Pub/Sub-side filter.
func TestSubscriptionPreservesTraceparent_PullModeAlwaysPreserved(t *testing.T) {
	sub := &pubsubSubscription{
		Name:  "projects/p/subscriptions/pull-only",
		Topic: "projects/p/topics/orders",
	}
	preserved, note := subscriptionPreservesTraceparent(sub)
	assert.True(t, preserved)
	assert.Empty(t, note)
}

// TestSubscriptionPreservesTraceparent_PushModeWithTraceparentKey_Preserved
// — a push-mode subscription whose Attributes filter INCLUDES the
// traceparent key (or a variant) preserves propagation.
func TestSubscriptionPreservesTraceparent_PushModeWithTraceparentKey_Preserved(t *testing.T) {
	sub := makePushSubscription("p", "s", "orders", map[string]string{
		"order_id":    "v",
		"traceparent": "v",
	})
	preserved, note := subscriptionPreservesTraceparent(sub)
	assert.True(t, preserved,
		"a filter that allows traceparent through preserves propagation")
	assert.Empty(t, note)
}
