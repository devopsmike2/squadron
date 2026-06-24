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

// ScanEventSources is the GCP scanner's event-source-tier entry
// point. Slice 1 chunk 2 only covers Pub/Sub; future slices may add
// other GCP event source primitives (Cloud Tasks, Eventarc).
//
// Mirrors the AWS scanner's ScanEventSources / ScanEventBridge layout
// and this scanner's ScanOrchestrations / ScanWorkflows layout: a
// standalone Scanner method returning snapshots directly rather than
// threading them through Scan().
//
// scope.AccountID overrides the per-snapshot AccountID; empty falls
// back to s.ProjectID. scope.Regions is ignored because Pub/Sub
// topics are project-global (see PubSubRegionGlobal).
//
// IAM contract per design doc §12: pubsub.topics.list. Read-only.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	return s.ScanPubSubTopics(ctx, scope)
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
func (s *Scanner) walkPubSubTopics(ctx context.Context, client *http.Client, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	var (
		out       []scanner.EventSourceInstanceSnapshot
		pageToken string
	)
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
			out = append(out, projectPubSubTopic(topic, accountID))
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
