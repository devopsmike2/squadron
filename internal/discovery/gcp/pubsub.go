// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Event source tier slice 1 chunk 2 (v0.89.101, #735 Stream 133) GCP
// Pub/Sub topic scanner.
//
// Ships the GCP Pub/Sub API walk that the GCP scanner dispatches via
// ScanEventSources when the event source tier is in the request's
// tier list. Mirrors the orchestration tier's ScanOrchestrations
// layout (workflows.go) and the AWS scanner's ScanEventBridge layout
// (internal/discovery/aws/eventbridge.go): a standalone Scanner
// method that returns snapshots directly rather than threading them
// through Scan(). Surfaces in Result.EventSources with Provider="gcp"
// and Surface="pubsub" so the proposer routes to pubsub-trace-enable
// / pubsub-schema-attach (chunk 5).
//
// Library choice — raw HTTP rather than google.golang.org/api/pubsub/v1.
// The generated pubsub/v1 client's Topic struct does NOT include the
// tracingConfig.samplingRatio field even though the underlying REST
// API exposes it. tracingConfig is the slice-1 HasTraceAxis primitive
// per docs/proposals/event-source-tier-slice1.md §3.2, so the SDK path
// would silently drop the detection field. The chunk-2 walk issues
// raw HTTP GETs against the same oauth-authenticated *http.Client the
// SDK-using walks share, parsing the response into a local pubsubTopic
// struct that includes tracingConfig + schemaSettings +
// messageStoragePolicy. Slice 2 may switch back to the SDK once
// Google publishes tracingConfig on the generated Topic.
//
// API surface: GET /v1/projects/{project}/topics (paginated via
// pageToken). OAuth scope: cloud-platform.read-only (same as
// Workflows / Cloud Functions); the runbook documents
// roles/pubsub.viewer as the project-level IAM grant.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// PubSubSamplingRatioMin is the minimum tracingConfig.samplingRatio
// value that qualifies as HasTraceAxis. Slice 1 treats any value
// strictly greater than this as trace-enabled. GCP Pub/Sub allows
// samplingRatio in [0, 1.0]; a value of 0 (or absent tracingConfig)
// means trace is disabled. Per design doc §12, a topic with no
// tracingConfig is indistinguishable from one explicitly set to 0;
// both produce HasTraceAxis=false in slice 1.
const PubSubSamplingRatioMin = 0.0

// PubSubEventSourceSurface is the Surface discriminator string for
// GCP Pub/Sub snapshots. The proposer's recommendation-kind prefix
// routing switches on "eventbridge" → AWS, "pubsub" → GCP,
// "servicebus" → Azure, "streaming" → OCI.
const PubSubEventSourceSurface = "pubsub"

// PubSubSourceTypeTopic is the SourceType discriminator string for
// Pub/Sub topics. Mirrors the per-cloud "bus" / "topic" / "queue" /
// "namespace" / "stream" SourceType convention documented on
// scanner.EventSourceInstanceSnapshot.
const PubSubSourceTypeTopic = "topic"

// PubSubRegionGlobal is the Region value the scanner stamps on every
// Pub/Sub snapshot. Pub/Sub topics are project-global; the
// messageStoragePolicy.allowedPersistenceRegions list constrains
// where messages MAY be stored but the topic resource itself has no
// home region. "global" is the operator-readable denormalization.
const PubSubRegionGlobal = "global"

// PubSubReadonlyScope is the OAuth scope the Pub/Sub API walk is
// authorized against. cloud-platform.read-only — same posture as
// Workflows / Cloud Functions (see WorkflowsReadonlyScope +
// CloudFunctionsPlatformScope). Runbook documents roles/pubsub.viewer
// as the project-level IAM grant.
const PubSubReadonlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"

// ServiceIDPubSub is the event-source-tier-slice1.md §3.2 service
// identifier the scanner reports against Result.FailedServices when
// the Pub/Sub walk produces a non-fatal error. Same unprefixed shape
// as ServiceIDComputeEngine / ServiceIDCloudSQL / ServiceIDGKE.
const ServiceIDPubSub = "pubsub"

// pubsubAPIBaseURL is the production Pub/Sub REST API root. Pub/Sub
// is a single-endpoint global API; locational constraints flow
// through messageStoragePolicy rather than per-region endpoints
// (unlike Cloud Run / Workflows). Tests override via s.endpoint.
const pubsubAPIBaseURL = "https://pubsub.googleapis.com"

// pubsubTopic is the chunk-2 raw-HTTP decode target for the Pub/Sub
// REST API's Topic resource. The generated pubsub/v1 client's Topic
// struct does not include tracingConfig (see file-level godoc on
// library choice); this local struct exposes that field plus the
// other slice-1 detection inputs (schemaSettings,
// messageStoragePolicy). TracingConfig is pointer-typed so the
// scanner can distinguish "absent" (nil) from "explicit zero"
// (non-nil) at the Detail-bag layer even though HasTraceAxis
// collapses them per §12.
type pubsubTopic struct {
	// Name is the fully-qualified topic resource path
	// "projects/{project}/topics/{topic}". Mapped to ResourceARN
	// verbatim; trailing segment becomes ResourceName.
	Name string `json:"name"`
	// TracingConfig drives HasTraceAxis.
	TracingConfig *pubsubTracingConfig `json:"tracingConfig,omitempty"`
	// SchemaSettings — Detail bag entry when Schema is set.
	SchemaSettings *pubsubSchemaSettings `json:"schemaSettings,omitempty"`
	// MessageStoragePolicy — Detail bag entry when regions populated.
	MessageStoragePolicy *pubsubMessageStoragePolicy `json:"messageStoragePolicy,omitempty"`
	// KmsKeyName — informational Detail entry when non-empty.
	KmsKeyName string `json:"kmsKeyName,omitempty"`
	// Labels — informational Detail entry when non-empty.
	Labels map[string]string `json:"labels,omitempty"`
}

// pubsubTracingConfig mirrors tracingConfig. SamplingRatio is the
// float in [0, 1.0] that controls publish-side Cloud Trace span
// emission. HasTraceAxis flips when SamplingRatio >
// PubSubSamplingRatioMin per design doc §3.2.
type pubsubTracingConfig struct {
	SamplingRatio float64 `json:"samplingRatio"`
}

// pubsubSchemaSettings mirrors schemaSettings. Slice 1 reads Schema
// to record the reference + Encoding in the Detail bag.
type pubsubSchemaSettings struct {
	Schema   string `json:"schema,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

// pubsubMessageStoragePolicy mirrors messageStoragePolicy. Slice 1
// reads AllowedPersistenceRegions to record the region list in the
// Detail bag (informational; pubsub-storage-policy is not a
// recommendation kind in slice 1 per design doc §3.2).
type pubsubMessageStoragePolicy struct {
	AllowedPersistenceRegions []string `json:"allowedPersistenceRegions,omitempty"`
}

// pubsubListTopicsResponse mirrors projects.topics.list. NextPageToken
// drives the pagination loop.
type pubsubListTopicsResponse struct {
	Topics        []*pubsubTopic `json:"topics,omitempty"`
	NextPageToken string         `json:"nextPageToken,omitempty"`
}

// PubSubTraceparentSchemaFieldPatterns is the case-insensitive substring
// set Squadron searches for in attached topic schemas to detect whether
// the schema includes a traceparent field. Any match → schema preserves
// trace propagation.
//
// See docs/proposals/event-source-tier-slice2.md §3.2.
//
// The three patterns cover the three field-name conventions seen in the
// wild: the W3C "traceparent" header name (the OpenTelemetry-native
// convention), the snake_case "trace_context" field (an SDK convention
// used by some publishers when the schema is Protobuf or Avro and the
// dot in "traceparent" would conflict with field-name validation), and
// the AWS-style "googclient_OpenTelemetryTraceparent" attribute key
// that Pub/Sub's client libraries emit when OTel instrumentation is on.
var PubSubTraceparentSchemaFieldPatterns = []string{
	"traceparent",
	"trace_context",
	"googclient_opentelemetrytraceparent",
}

// pubsubSchema mirrors the GCP Pub/Sub Schema resource. Slice 2 chunk 2
// reads Definition to apply schemaIncludesTraceparentField. Type +
// RevisionID surface in the Detail bag (informational; the slice-2
// detection rule only consumes Definition).
type pubsubSchema struct {
	// Name is the fully-qualified schema resource path
	// "projects/{project}/schemas/{schema}". Used as the cache key.
	Name string `json:"name"`
	// Definition is the raw schema body (Protobuf descriptor / Avro
	// JSON). Slice 2 detection inspects this string for the
	// PubSubTraceparentSchemaFieldPatterns substrings.
	Definition string `json:"definition,omitempty"`
	// Type is the schema type: "PROTOCOL_BUFFER" or "AVRO".
	// Informational; the detection rule does not consume it.
	Type string `json:"type,omitempty"`
}

// pubsubSubscription mirrors the GCP Pub/Sub Subscription resource.
// Slice 2 chunk 2 inspects Topic (to match against the discovered
// topic list) and PushConfig (to apply the subscription delivery axis
// detection per design doc §3.2).
type pubsubSubscription struct {
	// Name is the fully-qualified subscription resource path
	// "projects/{project}/subscriptions/{sub}". The trailing segment
	// is used in propagation notes for operator readability.
	Name string `json:"name"`
	// Topic is the fully-qualified topic resource path the
	// subscription is attached to. The scanner matches this against
	// the discovered topic list to apply per-topic aggregation.
	Topic string `json:"topic,omitempty"`
	// PushConfig is populated for push-mode subscriptions. Slice 2
	// detection inspects the Attributes map for an explicit
	// attribute filter; when set AND non-empty AND missing every
	// traceparent variant, propagation is BROKEN.
	PushConfig *pubsubPushConfig `json:"pushConfig,omitempty"`
}

// pubsubPushConfig mirrors pushConfig. Slice 2 chunk 2 reads the
// Attributes map; per design doc §3.2 a non-empty Attributes filter
// that omits every traceparent variant breaks propagation for the
// parent topic.
//
// Per the GCP API contract, an empty pushEndpoint signals a pull-mode
// subscription — slice 2 treats those as propagation-preserved because
// the consumer SDK reads all message attributes verbatim. The
// push-config attribute axis only matters for push-mode subscriptions.
type pubsubPushConfig struct {
	PushEndpoint string            `json:"pushEndpoint,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// pubsubListSubscriptionsResponse mirrors projects.subscriptions.list.
// NextPageToken drives pagination; the subscription list is project-
// wide and matched against the discovered topic list at the call site.
type pubsubListSubscriptionsResponse struct {
	Subscriptions []*pubsubSubscription `json:"subscriptions,omitempty"`
	NextPageToken string                `json:"nextPageToken,omitempty"`
}

// schemaCache is the per-scan cache for Pub/Sub Schema fetches. Slice 2
// chunk 2 amortizes schema fetches across topics: a single schema
// referenced by N topics is fetched once. The cache is intentionally
// PER-SCAN (constructed inside ScanPubSubTopics) rather than long-lived
// on the Scanner — schemas can be edited between scans, and a stale
// cache would mask a schema fix the operator just shipped. The chunk-2
// per-scan scope mirrors the AWS scanner's per-scan rate-limiter
// posture: short-lived scratchpad state, never carried across scans.
//
// fetchedNotFound tracks per-schema fetch failures so the topic-axis
// computation can emit an informational note (the topic still surfaces
// with axis=true since the failure is non-fatal). results[name] = true
// when the fetched definition includes a traceparent field per
// schemaIncludesTraceparentField; false when the fetch succeeded and
// the field is absent. Absent map entries mean "not yet fetched".
type schemaCache struct {
	mu              sync.Mutex
	results         map[string]bool   // schemaName → includes-traceparent
	fetchedNotFound map[string]string // schemaName → fetch error message
}

// newSchemaCache constructs the per-scan cache. Called once at the top
// of walkPubSubTopics.
func newSchemaCache() *schemaCache {
	return &schemaCache{
		results:         map[string]bool{},
		fetchedNotFound: map[string]string{},
	}
}

// schemaIncludesTraceparentField returns (included, fieldName).
// fieldName is the first matching pattern when included is true,
// or empty when not. The detection is a case-insensitive substring
// match against the patterns in PubSubTraceparentSchemaFieldPatterns —
// the design doc §3.2 names substring (not exact-field-name) match so
// the rule covers Protobuf / Avro / JSON schemas without separate
// parsers per schema-language.
//
// An empty schema definition returns (false, "") — vacuously not
// included, which the per-topic aggregation treats as a propagation
// gap when the topic has the schema attached.
func schemaIncludesTraceparentField(schemaDefinition string) (bool, string) {
	lower := strings.ToLower(schemaDefinition)
	for _, pattern := range PubSubTraceparentSchemaFieldPatterns {
		if strings.Contains(lower, pattern) {
			return true, pattern
		}
	}
	return false, ""
}

// ScanEventSources is the GCP scanner's event-source-tier entry
// point. Slice 1 chunk 2 (v0.89.101) shipped Pub/Sub alone; slice 5
// chunk 1 (v0.89.144, #784 Stream 182) extends the dispatcher to fan
// out across BOTH Pub/Sub topics AND Cloud Tasks queues with a
// two-way partial-scan posture mirroring the AWS slice 4 three-way
// dispatcher (internal/discovery/aws/eventbridge.go::ScanEventSources).
//
// Partial-scan posture per docs/proposals/event-source-tier-slice5.md
// §5: when ONE of the two surfaces fails (e.g. an IAM gap on the
// Cloud Tasks read permissions while the Pub/Sub grant is already
// wired) the REMAINING surface still surfaces. Only when BOTH
// surfaces fail does the dispatcher return a non-nil error wrapping
// every per-surface error so the operator sees the full failure
// envelope. The §12 threat model treats this as load-bearing: the
// dispatcher's both-directions partial-scan posture is pinned by
// acceptance tests 10 / 11 / 12 / 13 of the slice 5 design doc.
//
// scope.AccountID overrides the per-snapshot AccountID; empty falls
// back to s.ProjectID. scope.Regions controls the Cloud Tasks walk's
// per-region iteration; Pub/Sub topics are project-global and ignore
// the region list (see PubSubRegionGlobal).
//
// IAM contract per design doc §12:
//   - pubsub.topics.list
//   - cloudtasks.queues.list + cloudtasks.queues.get
//
// All read-only. Squadron never executes a Pub/Sub or Cloud Tasks
// mutation API.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	var all []scanner.EventSourceInstanceSnapshot

	topics, pubsubErr := s.ScanPubSubTopics(ctx, scope)
	if pubsubErr == nil {
		all = append(all, topics...)
	}

	queues, ctErr := s.ScanCloudTasksQueues(ctx, scope)
	if ctErr == nil {
		all = append(all, queues...)
	}

	// Slice 10 chunk 1 (v0.89.159, #801 Stream 198) extends the GCP
	// dispatcher to three-way (Pub/Sub + Cloud Tasks + Pub/Sub Lite).
	// Pub/Sub Lite is GCP's partitioned-log primitive, the structural
	// analog of AWS Kinesis Data Streams and Azure Event Hubs.
	// CLOSES the cross-cloud event source widening pass at 3-3-3-3 /
	// 12 surfaces across 4 clouds. See
	// docs/proposals/event-source-tier-slice10.md §5.
	liteTopics, pslErr := s.ScanPubSubLiteTopics(ctx, scope)
	if pslErr == nil {
		all = append(all, liteTopics...)
	}

	// Three-way partial-scan posture: only return an error when ALL
	// THREE surfaces failed. Any one- OR two-surface failure is
	// silenced at this layer. Combinatorial single-failure paths are
	// pinned by slice 10 acceptance tests 8 + 9 + 10; two-of-three
	// failure path by test 11; all-three-fail error-string contract
	// by test 12. Mirrors slice 8 Azure + slice 9 OCI three-way
	// dispatchers.
	if pubsubErr != nil && ctErr != nil && pslErr != nil {
		return all, fmt.Errorf("all gcp event source surfaces failed: pubsub=%v cloudtasks=%v pubsublite=%w", pubsubErr, ctErr, pslErr)
	}

	return all, nil
}

// ScanPubSubTopics walks the configured project's Pub/Sub topics and
// returns the mapped event source snapshots. Slice 1 chunk 2 of the
// event-source-tier arc (v0.89.101, #735 Stream 133).
//
// Detection per design doc §3.2:
//   - HasTraceAxis ← tracingConfig.samplingRatio >
//     PubSubSamplingRatioMin (strictly > 0.0). Absent tracingConfig
//     is treated as samplingRatio=0; the §12 "absent vs. zero"
//     ambiguity is an accepted slice-1 imprecision.
//   - HasLogAxis stays false. Pub/Sub does not expose a topic-level
//     structured-logging destination; logging is configured on the
//     consumer subscription side (slice 2 candidate per §13).
//     schemaSettings flips no axis — it's a payload-shape signal,
//     surfaced in the Detail bag for the pubsub-schema-attach
//     recommendation kind.
//
// Detail bag fields (per §3.2): schema / schema_encoding /
// allowed_persistence_regions / tracing_sampling_ratio (only when
// tracingConfig is non-nil, preserving the "absent vs. zero"
// distinction at the drilldown layer) / kms_key_name / labels.
//
// IAM: pubsub.topics.list. The list endpoint returns full Topic
// resources (including tracingConfig + schemaSettings +
// messageStoragePolicy) so per-topic Get calls are not needed.
func (s *Scanner) ScanPubSubTopics(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	if s.ProjectID == "" {
		return nil, errors.New("gcp: ProjectID is required")
	}
	if len(s.SAJSON) == 0 && s.httpClient == nil {
		return nil, errors.New("gcp: SAJSON is required")
	}
	client, err := s.buildPubSubHTTPClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: build pubsub http client: %w", err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.ProjectID
	}
	return s.walkPubSubTopics(ctx, client, accountID)
}

// walkPubSubTopics issues the paginated GET against
// /v1/projects/{project}/topics, decodes each response into the
// local pubsubTopic struct, and projects the result into
// EventSourceInstanceSnapshot rows. Per-page failures abort the walk
// and propagate as an error — Pub/Sub's list is single-endpoint so
// there's no per-region walk to fall through to; the handler-side
// dispatcher records a partial-failure entry against
// ServiceIDPubSub.
//
// Slice 2 chunk 2 (v0.89.106, #742 Stream 140) extends the walk with
// two new sub-axes: (1) schema field inspection against attached topic
// schemas, and (2) subscription delivery inspection across the
// project's subscription set. The subscription list is fetched once
// per scan (before the topic walk) and indexed by topic ARN so each
// topic's per-topic aggregation can read the matching subscriptions
// without an extra API call per topic. The schema cache (per-scan;
// see schemaCache godoc) amortizes schema fetches across topics that
// reference the same schema. Per design doc §3.2 the per-topic
// HasPropagationConfig is true ONLY when BOTH sub-axes (schema +
// subscription) are preserved; either axis failing flips it false.
func (s *Scanner) walkPubSubTopics(ctx context.Context, client *http.Client, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	var (
		out       []scanner.EventSourceInstanceSnapshot
		pageToken string
	)
	// Slice 2 chunk 2: list every subscription in the project once,
	// index by topic ARN. The per-topic aggregation reads the matching
	// list when computing the subscription axis. A failure here is
	// non-fatal: the topic walk proceeds with an empty subscription
	// index (subscription axis defaults to preserved per design doc
	// §3.2 — no subscriptions observed = nothing to break propagation).
	subsByTopic := s.listSubscriptionsByTopic(ctx, client)
	// Slice 2 chunk 2: per-scan schema cache. Each schema is fetched
	// at most once per scan even when referenced by many topics.
	cache := newSchemaCache()
	for {
		listURL := s.pubsubListTopicsURL(pageToken)
		resp, err := s.pubsubGet(ctx, client, listURL)
		if err != nil {
			return out, err
		}
		for _, topic := range resp.Topics {
			if topic == nil || topic.Name == "" {
				continue
			}
			snap := projectPubSubTopic(topic, accountID)
			s.applyPubSubPropagation(ctx, client, cache, topic, subsByTopic[topic.Name], &snap)
			out = append(out, snap)
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.NextPageToken
	}
}

// pubsubListTopicsURL constructs the full URL for projects.topics.list.
// Uses s.endpoint when set (tests) or pubsubAPIBaseURL (production).
// pageToken is added as a query parameter when non-empty.
func (s *Scanner) pubsubListTopicsURL(pageToken string) string {
	base := s.endpoint
	if base == "" {
		base = pubsubAPIBaseURL
	}
	u := fmt.Sprintf("%s/v1/projects/%s/topics", strings.TrimRight(base, "/"), s.ProjectID)
	if pageToken != "" {
		u = u + "?pageToken=" + url.QueryEscape(pageToken)
	}
	return u
}

// pubsubListSubscriptionsURL constructs the full URL for
// projects.subscriptions.list. Slice 2 chunk 2 (v0.89.106). Mirrors
// pubsubListTopicsURL's shape.
func (s *Scanner) pubsubListSubscriptionsURL(pageToken string) string {
	base := s.endpoint
	if base == "" {
		base = pubsubAPIBaseURL
	}
	u := fmt.Sprintf("%s/v1/projects/%s/subscriptions", strings.TrimRight(base, "/"), s.ProjectID)
	if pageToken != "" {
		u = u + "?pageToken=" + url.QueryEscape(pageToken)
	}
	return u
}

// pubsubGetSchemaURL constructs the full URL for projects.schemas.get.
// schemaName is the fully-qualified path returned by
// pubsubTopic.SchemaSettings.Schema ("projects/{p}/schemas/{s}"). The
// ?view=FULL query param is required to receive the schema Definition
// — the default BASIC view omits it per the GCP API contract.
func (s *Scanner) pubsubGetSchemaURL(schemaName string) string {
	base := s.endpoint
	if base == "" {
		base = pubsubAPIBaseURL
	}
	return fmt.Sprintf("%s/v1/%s?view=FULL",
		strings.TrimRight(base, "/"),
		strings.TrimLeft(schemaName, "/"))
}

// pubsubGet issues a single GET against the supplied URL and decodes
// the JSON body. Non-2xx responses surface as an error with the
// operator-readable classification (403 / 404 / 429 / other);
// mirrors classifyWorkflowsListError with the pubsub.viewer
// remediation hint per design doc §12.
func (s *Scanner) pubsubGet(ctx context.Context, client *http.Client, listURL string) (*pubsubListTopicsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %s", ServiceIDPubSub, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", ServiceIDPubSub, classifyPubSubTransportError(err))
	}
	defer httpResp.Body.Close()
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("%s: read response body: %s", ServiceIDPubSub, truncate(readErr.Error(), 200))
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", ServiceIDPubSub, classifyPubSubListStatus(httpResp.StatusCode, body))
	}
	var resp pubsubListTopicsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%s: decode response: %s", ServiceIDPubSub, truncate(err.Error(), 200))
	}
	return &resp, nil
}

// projectPubSubTopic maps a pubsubTopic into the provider-agnostic
// EventSourceInstanceSnapshot per design doc §3.2 + §5:
//   - Provider="gcp" / Surface="pubsub" / SourceType="topic"
//   - AccountID = project id; Region = "global" (see PubSubRegionGlobal)
//   - ResourceARN = topic.Name verbatim; ResourceName = trailing segment
//   - HasTraceAxis ← tracingConfig != nil AND
//     samplingRatio > PubSubSamplingRatioMin (0.0)
//   - HasLogAxis ← false (see ScanPubSubTopics godoc)
//   - Detail bag carries schema / schema_encoding / regions /
//     tracing_sampling_ratio / kms / labels per Inventory drilldown.
func projectPubSubTopic(topic *pubsubTopic, accountID string) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:    string(credstore.ProviderGCP),
		Surface:     PubSubEventSourceSurface,
		AccountID:   accountID,
		Region:      PubSubRegionGlobal,
		ResourceARN: topic.Name,
		SourceType:  PubSubSourceTypeTopic,
	}
	if topic.Name != "" {
		if i := strings.LastIndex(topic.Name, "/"); i >= 0 && i < len(topic.Name)-1 {
			snap.ResourceName = topic.Name[i+1:]
		} else {
			snap.ResourceName = topic.Name
		}
	}
	if topic.TracingConfig != nil && topic.TracingConfig.SamplingRatio > PubSubSamplingRatioMin {
		snap.HasTraceAxis = true
	}
	snap.Detail = buildPubSubDetail(topic)
	return snap
}

// buildPubSubDetail folds the per-topic raw fields into the Detail
// bag. Empty fields are omitted; nil returned when no detail applies.
// The bag carries raw detection inputs (schema, regions,
// samplingRatio) alongside informational fields (labels, kms) so the
// drilldown has both the axis reasoning AND broader context without
// a second API call.
func buildPubSubDetail(topic *pubsubTopic) map[string]any {
	detail := map[string]any{}
	if topic.SchemaSettings != nil && topic.SchemaSettings.Schema != "" {
		detail["schema"] = topic.SchemaSettings.Schema
		if topic.SchemaSettings.Encoding != "" {
			detail["schema_encoding"] = topic.SchemaSettings.Encoding
		}
	}
	if topic.MessageStoragePolicy != nil && len(topic.MessageStoragePolicy.AllowedPersistenceRegions) > 0 {
		// Defensive copy — the Detail map outlives the response.
		regions := make([]string, len(topic.MessageStoragePolicy.AllowedPersistenceRegions))
		copy(regions, topic.MessageStoragePolicy.AllowedPersistenceRegions)
		detail["allowed_persistence_regions"] = regions
	}
	if topic.TracingConfig != nil {
		// Raw samplingRatio surfaces even at 0 so the drilldown can
		// distinguish "absent" (no entry) from "explicit zero"
		// (entry = 0.0). HasTraceAxis collapses both per §12; the
		// Detail bag preserves the distinction honestly.
		detail["tracing_sampling_ratio"] = topic.TracingConfig.SamplingRatio
	}
	if topic.KmsKeyName != "" {
		detail["kms_key_name"] = topic.KmsKeyName
	}
	if len(topic.Labels) > 0 {
		labels := make(map[string]string, len(topic.Labels))
		for k, v := range topic.Labels {
			labels[k] = v
		}
		detail["labels"] = labels
	}
	if len(detail) == 0 {
		return nil
	}
	return detail
}

// buildPubSubHTTPClient returns the *http.Client the chunk-2 walk
// issues raw GETs against. Test path: s.httpClient verbatim.
// Production: reuses the shared oauth-backed client from
// buildOAuthHTTPClient so the SA JSON parse + scope union is
// computed once per scan. Error message NEVER embeds SA JSON bytes
// per the credstore substrate invariant.
func (s *Scanner) buildPubSubHTTPClient(ctx context.Context) (*http.Client, error) {
	if s.httpClient != nil {
		return s.httpClient, nil
	}
	oauthClient, err := s.buildOAuthHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	if oauthClient == nil {
		// Defensive: buildOAuthHTTPClient returns (nil, nil) only on
		// the test-bypass path gated by s.httpClient above.
		return nil, errors.New("gcp: nil oauth client (no SA JSON and no test client)")
	}
	return oauthClient, nil
}

// classifyPubSubListStatus maps a non-2xx response to an operator-
// readable PartialReason string. Same shape as
// classifyWorkflowsListError but driven off the raw HTTP status code
// rather than a googleapi.Error. Body parsing is best-effort.
//
// 403 → permission denied (roles/pubsub.viewer hint).
// 404 → project not found.
// 429 → rate limit.
// Other → truncated message with HTTP code.
func classifyPubSubListStatus(code int, body []byte) string {
	switch code {
	case http.StatusForbidden:
		return "permission denied (verify the service account has roles/pubsub.viewer)"
	case http.StatusNotFound:
		return "project not found (verify project_id is correct)"
	case http.StatusTooManyRequests:
		return "rate limit exceeded mid-scan"
	default:
		msg := extractPubSubErrorMessage(body)
		return fmt.Sprintf("topics list failed (HTTP %d): %s", code, truncate(msg, 200))
	}
}

// classifyPubSubTransportError maps a transport / network failure
// (DNS, TCP, TLS, context cancellation) into the operator-readable
// PartialReason string. Mirrors classifyWorkflowsListError's
// transport branch.
func classifyPubSubTransportError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("network error: %s", truncate(err.Error(), 200))
}

// extractPubSubErrorMessage best-effort decodes the standard GCP
// error envelope. Falls back to the raw body when the envelope
// doesn't match (httptest fakes may return plain bodies).
func extractPubSubErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return string(body)
}

// listSubscriptionsByTopic fetches every subscription in the project
// and groups them by their parent topic ARN. Slice 2 chunk 2 of the
// Event source tier arc (v0.89.106, #742 Stream 140).
//
// The map is keyed by the fully-qualified topic resource path
// (matching pubsubTopic.Name) so the per-topic aggregation in
// walkPubSubTopics can look up matching subscriptions with a single
// map read. Subscriptions whose Topic field is empty or pointing at
// a topic outside the discovered list are still grouped — the
// per-topic aggregation simply ignores misses.
//
// A failure here is non-fatal: the function returns an empty map and
// swallows the error. The per-topic subscription axis defaults to
// preserved (no subscriptions observed = nothing to break
// propagation per design doc §3.2). This keeps the slice-2 walk from
// blocking the slice-1 topic inventory when the IAM grant has
// pubsub.topics.list but not pubsub.subscriptions.list — operators
// see the topic rows even when the subscription axis can't be
// computed.
func (s *Scanner) listSubscriptionsByTopic(ctx context.Context, client *http.Client) map[string][]*pubsubSubscription {
	out := map[string][]*pubsubSubscription{}
	var pageToken string
	for {
		listURL := s.pubsubListSubscriptionsURL(pageToken)
		resp, err := s.pubsubGetSubscriptions(ctx, client, listURL)
		if err != nil {
			return out
		}
		for _, sub := range resp.Subscriptions {
			if sub == nil || sub.Topic == "" {
				continue
			}
			out[sub.Topic] = append(out[sub.Topic], sub)
		}
		if resp.NextPageToken == "" {
			return out
		}
		pageToken = resp.NextPageToken
	}
}

// pubsubGetSubscriptions issues a single GET against the supplied URL
// and decodes the subscription list. Slice 2 chunk 2 mirror of
// pubsubGet but typed for the subscription list response shape.
func (s *Scanner) pubsubGetSubscriptions(ctx context.Context, client *http.Client, listURL string) (*pubsubListSubscriptionsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %s", ServiceIDPubSub, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", ServiceIDPubSub, classifyPubSubTransportError(err))
	}
	defer httpResp.Body.Close()
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("%s: read response body: %s", ServiceIDPubSub, truncate(readErr.Error(), 200))
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", ServiceIDPubSub, classifyPubSubListStatus(httpResp.StatusCode, body))
	}
	var resp pubsubListSubscriptionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%s: decode response: %s", ServiceIDPubSub, truncate(err.Error(), 200))
	}
	return &resp, nil
}

// pubsubGetSchema fetches a single Pub/Sub Schema by fully-qualified
// resource path. Slice 2 chunk 2. The view=FULL query param is added
// by pubsubGetSchemaURL so the response body carries the schema
// Definition the propagation detection rule reads.
func (s *Scanner) pubsubGetSchema(ctx context.Context, client *http.Client, schemaName string) (*pubsubSchema, error) {
	schemaURL := s.pubsubGetSchemaURL(schemaName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, schemaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %s", ServiceIDPubSub, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", ServiceIDPubSub, classifyPubSubTransportError(err))
	}
	defer httpResp.Body.Close()
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("%s: read response body: %s", ServiceIDPubSub, truncate(readErr.Error(), 200))
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", ServiceIDPubSub, classifyPubSubListStatus(httpResp.StatusCode, body))
	}
	var schema pubsubSchema
	if err := json.Unmarshal(body, &schema); err != nil {
		return nil, fmt.Errorf("%s: decode response: %s", ServiceIDPubSub, truncate(err.Error(), 200))
	}
	return &schema, nil
}

// schemaCacheLookup returns the cached schemaIncludesTraceparentField
// result for the supplied schema. Returns (included, fetched, fetchErr)
// where fetched=false signals "not yet in the cache — caller should
// fetch and call schemaCacheStore". fetchErr is the previously recorded
// fetch error when one happened; the caller emits an informational note
// without re-fetching.
func (c *schemaCache) lookup(schemaName string) (included bool, fetched bool, fetchErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if errMsg, ok := c.fetchedNotFound[schemaName]; ok {
		return false, true, errMsg
	}
	if v, ok := c.results[schemaName]; ok {
		return v, true, ""
	}
	return false, false, ""
}

// schemaCacheStore records the schemaIncludesTraceparentField outcome
// against the schema name. Subsequent topics referencing the same
// schema read the cached result via schemaCacheLookup.
func (c *schemaCache) store(schemaName string, included bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results[schemaName] = included
}

// schemaCacheStoreError records a fetch failure against the schema
// name. The error message is held verbatim so the per-topic note can
// surface the operator-readable cause.
func (c *schemaCache) storeError(schemaName, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetchedNotFound[schemaName] = msg
}

// applyPubSubPropagation computes the slice-2 chunk-2 per-topic
// propagation axis and writes the result onto the supplied snapshot.
// Per design doc §3.2 the per-topic HasPropagationConfig is true ONLY
// when BOTH sub-axes (schema + subscription) are preserved.
//
// Schema axis (design doc §3.2 first detection rule):
//   - Topic has no schemaSettings.schema → PRESERVED (publisher
//     controls; no schema enforcement to drop the traceparent attr).
//   - Topic has schemaSettings.schema set + schema fetch returns
//     definition that matches one of the
//     PubSubTraceparentSchemaFieldPatterns → PRESERVED.
//   - Topic has schemaSettings.schema set + schema fetch returns
//     definition that matches NO pattern → BROKEN. PropagationNotes
//     records the schema name + the missing-field reason.
//   - Topic has schemaSettings.schema set + schema fetch FAILS →
//     PRESERVED with an informational note (the failure is non-fatal;
//     the operator sees the topic + the schema-fetch error and can
//     fix the IAM grant or the schema reference).
//
// Subscription axis (design doc §3.2 second detection rule):
//   - Topic has no subscriptions → PRESERVED (vacuously; nothing to
//     break propagation).
//   - Every subscription on the topic preserves propagation →
//     PRESERVED.
//   - Any subscription on the topic has a push-mode delivery + a
//     non-empty Attributes filter that omits every traceparent
//     variant → BROKEN. PropagationNotes records the subscription
//     name + the offending filter.
func (s *Scanner) applyPubSubPropagation(
	ctx context.Context,
	client *http.Client,
	cache *schemaCache,
	topic *pubsubTopic,
	subs []*pubsubSubscription,
	snap *scanner.EventSourceInstanceSnapshot,
) {
	preserved := true
	var notes []string

	// --- Schema axis ---
	if topic.SchemaSettings != nil && topic.SchemaSettings.Schema != "" {
		schemaName := topic.SchemaSettings.Schema
		included, fetched, fetchErr := cache.lookup(schemaName)
		if !fetched {
			schema, err := s.pubsubGetSchema(ctx, client, schemaName)
			if err != nil {
				// Non-fatal: record the error against the cache and emit
				// an informational note. The schema axis stays preserved
				// — Squadron declines to emit a false-positive
				// recommendation against a schema it couldn't read.
				cache.storeError(schemaName, truncate(err.Error(), 160))
				notes = append(notes, fmt.Sprintf(
					"topic schema %q fetch failed (axis assumed preserved): %s",
					schemaName, truncate(err.Error(), 160)))
			} else {
				def := ""
				if schema != nil {
					def = schema.Definition
				}
				inc, _ := schemaIncludesTraceparentField(def)
				cache.store(schemaName, inc)
				included = inc
				fetched = true
			}
		} else if fetchErr != "" {
			// Cached fetch failure from an earlier topic on this scan.
			// Emit the same informational note; axis stays preserved.
			notes = append(notes, fmt.Sprintf(
				"topic schema %q fetch failed (axis assumed preserved): %s",
				schemaName, fetchErr))
		}
		// fetched=true with no fetchErr → consume the included verdict.
		if fetched && fetchErr == "" && !included {
			preserved = false
			notes = append(notes, fmt.Sprintf(
				"topic schema %q missing traceparent field", schemaName))
		}
	}

	// --- Subscription axis ---
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		ok, note := subscriptionPreservesTraceparent(sub)
		if !ok {
			preserved = false
			if note != "" {
				notes = append(notes, note)
			}
		}
	}

	snap.HasPropagationConfig = preserved
	if len(notes) > 0 {
		snap.PropagationNotes = notes
	}
}

// subscriptionPreservesTraceparent applies the slice-2 chunk-2
// per-subscription detection rule. Returns (preserved, note).
//
// Rule (design doc §3.2):
//   - Pull-mode subscription (no pushConfig OR pushConfig with empty
//     pushEndpoint) → PRESERVED. The consumer SDK reads all message
//     attributes verbatim; there's no Pub/Sub-side filter to drop
//     the traceparent attribute.
//   - Push-mode subscription with no Attributes filter → PRESERVED.
//     The push endpoint receives every message attribute.
//   - Push-mode subscription with a non-empty Attributes filter that
//     INCLUDES at least one traceparent variant → PRESERVED.
//   - Push-mode subscription with a non-empty Attributes filter that
//     OMITS every traceparent variant → BROKEN. Note names the
//     subscription + the cause for the proposer's reasoning text.
func subscriptionPreservesTraceparent(sub *pubsubSubscription) (bool, string) {
	if sub == nil || sub.PushConfig == nil || sub.PushConfig.PushEndpoint == "" {
		return true, ""
	}
	attrs := sub.PushConfig.Attributes
	if len(attrs) == 0 {
		return true, ""
	}
	for key := range attrs {
		lowerKey := strings.ToLower(key)
		for _, pattern := range PubSubTraceparentSchemaFieldPatterns {
			if strings.Contains(lowerKey, pattern) {
				return true, ""
			}
		}
	}
	subName := sub.Name
	if i := strings.LastIndex(subName, "/"); i >= 0 && i < len(subName)-1 {
		subName = subName[i+1:]
	}
	return false, fmt.Sprintf(
		"subscription %q has attribute filter excluding traceparent", subName)
}
