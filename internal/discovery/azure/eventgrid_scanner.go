// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Event source tier slice 6 chunk 1 constants — see
// docs/proposals/event-source-tier-slice6.md §3 (detection axes) and
// §11 acceptance tests 1-13. Mirrors the shape of the slice 1 chunk 3
// Service Bus constants block (v0.89.101): per-surface API version pin,
// shared diagnostic-settings API version (the microsoft.insights/
// diagnosticSettings sub-resource is the same across Azure resource
// providers), per-service identifier, surface discriminator string,
// and the canonical SourceType.
const (
	// EventGridTopicsAPIVersion pins the ARM API version slice 6 uses
	// for the Microsoft.EventGrid/topics subscription-wide list and
	// per-topic get calls. The 2025-02-15 surface is the stable
	// general-availability version that exposes the inputSchema +
	// publicNetworkAccess + disableLocalAuth properties slice 6 reads.
	EventGridTopicsAPIVersion = "2025-02-15"

	// EventGridDiagnosticSettingsAPIVersion mirrors the slice 1 Service
	// Bus chunk 3 ServiceBusDiagnosticSettingsAPIVersion (v0.89.101) —
	// the microsoft.insights/diagnosticSettings surface is the shared
	// Azure observability-routing primitive and every per-resource
	// scanner pins the same 2021-05-01-preview version. Re-declared
	// here (rather than aliased) so the slice 6 chunk 1 grep is
	// self-contained — a future slice that promotes the diagnostic-
	// settings API to GA can lift both constants in lockstep without
	// surprising the per-resource scanners.
	EventGridDiagnosticSettingsAPIVersion = "2021-05-01-preview"

	// ServiceIDEventGrid is the per-service identifier the scanner
	// reports against Result.FailedServices when the Event Grid walk
	// produces a non-fatal error. Mirrors the slice 1 chunk 3
	// ServiceIDServiceBus pattern — bare per-cloud-service identifier
	// (the provider discriminator is carried on the connection
	// envelope).
	ServiceIDEventGrid = "eventgrid"

	// EventGridSurface drives the proposer's recommendation-kind
	// prefix routing for Azure Event Grid event sources: eventgrid-* →
	// Azure (see docs/proposals/event-source-tier-slice6.md §8). The
	// snapshot Surface field carries this value verbatim.
	EventGridSurface = "eventgrid"

	// eventGridSourceTypeTopic is the SourceType discriminator string
	// for Event Grid Custom Topic rows. Slice 6 chunk 1 surfaces
	// Custom Topics only; Event Grid Domains + System Topics are
	// slice 7+ candidates per the design doc §2 non-goals list.
	eventGridSourceTypeTopic = "topic"

	// EventGridCloudEventSchemaV1 is the canonical W3C-standard
	// CloudEvents 1.0 schema identifier the slice 6 chunk 1 trace
	// axis detection rule keys on. The string is case-sensitive — the
	// Azure ARM response body returns "CloudEventSchemaV1_0" verbatim
	// and the comparison is exact-match per docs/proposals/
	// event-source-tier-slice6.md §3. The "EventGridSchema" (Azure
	// proprietary) and "CustomEventSchema" (operator-defined) values
	// do NOT satisfy the axis — they lack the CloudEvents 1.0
	// distributed-tracing extension that carries traceparent across
	// the topic's subscriptions.
	EventGridCloudEventSchemaV1 = "CloudEventSchemaV1_0"
)

// armEventGridTopicListResponse is the JSON shape returned by the
// subscription-wide Microsoft.EventGrid/topics list call. Slice 6
// chunk 1 (v0.89.147, #787 Stream 185) — mirrors
// armServiceBusNamespaceListResponse from slice 1 chunk 3 (v0.89.101);
// NextLink follows the standard ARM pagination convention.
type armEventGridTopicListResponse struct {
	Value    []armEventGridTopic `json:"value"`
	NextLink string              `json:"nextLink,omitempty"`
}

// armEventGridTopic is the bare JSON shape of a single
// Microsoft.EventGrid/topics entry. The slice 6 chunk 1 projection
// reads ID / Name / Location / Properties; other top-level fields
// (Type, Tags, SystemData) are intentionally untyped — slice 7 may
// extend the projection if operator feedback warrants.
type armEventGridTopic struct {
	ID         string                       `json:"id"`
	Name       string                       `json:"name"`
	Location   string                       `json:"location"`
	Tags       map[string]string            `json:"tags,omitempty"`
	Type       string                       `json:"type,omitempty"`
	Properties armEventGridTopicProperties  `json:"properties,omitempty"`
}

// armEventGridTopicProperties carries the slice 6 chunk 1 detection
// fields. InputSchema is the trace-axis discriminator; the other
// fields (publicNetworkAccess, disableLocalAuth, provisioningState)
// are informational-only per docs/proposals/event-source-tier-slice6.md
// §3 — surfaced in the snapshot Detail bag for operator context.
type armEventGridTopicProperties struct {
	InputSchema         string `json:"inputSchema,omitempty"`
	ProvisioningState   string `json:"provisioningState,omitempty"`
	PublicNetworkAccess string `json:"publicNetworkAccess,omitempty"`
	DisableLocalAuth    bool   `json:"disableLocalAuth,omitempty"`
}

// ScanEventGridTopics is the slice 6 chunk 1 entry point for the Azure
// Event Grid surface. Lists Microsoft.EventGrid/topics at subscription
// scope, probes per-topic microsoft.insights/diagnosticSettings, and
// returns []EventSourceInstanceSnapshot rows with Surface="eventgrid".
//
// Scope semantics mirror the slice 1 chunk 3 ScanEventSources:
// scope.Regions is ignored — Event Grid topics are subscription-scope
// on the ARM list endpoint. scope.AccountID overrides the per-row
// AccountID stamped on every snapshot; empty falls back to
// s.SubscriptionID.
//
// Detection axes per docs/proposals/event-source-tier-slice6.md §3:
//
//   - HasLogAxis  ← the namespace has a microsoft.insights/
//     diagnosticSettings child that routes to either an Application
//     Insights resource (properties.applicationInsights.connectionString)
//     OR a Log Analytics workspace (properties.workspaceId). Mirrors
//     the Service Bus slice 1 chunk 3 detection helper.
//   - HasTraceAxis ← properties.inputSchema == "CloudEventSchemaV1_0"
//     (the W3C-standard CloudEvents 1.0 schema, which includes the
//     distributed-tracing extension carrying traceparent across
//     subscriptions). EventGridSchema (Azure proprietary) and
//     CustomEventSchema (operator-defined) do NOT satisfy the axis.
//
// IAM contract per docs/proposals/event-source-tier-slice6.md §12: the
// existing Reader role at subscription scope covers BOTH the
// Microsoft.EventGrid/topics list AND the per-topic
// microsoft.insights/diagnosticSettings sub-resource read — no IAM
// extension beyond what slice 1 provided.
//
// Returns (nil, nil) when the scanner has not been wired with a
// SubscriptionID — graceful posture matches the slice 1 chunk 3
// ScanEventSources contract (the scanner is a constructed value; the
// caller is responsible for full configuration).
//
// 404 on the per-topic diagnostic-settings GET is the canonical "no
// settings configured" shape — HasLogAxis stays false, no partial
// failure recorded. Any other diagnostic-settings call failure leaves
// HasLogAxis false but still surfaces the topic row — the operator
// should see the inventory even when the per-axis detection failed.
// This mirrors the Service Bus slice 1 chunk 3 partial-failure
// posture.
func (s *Scanner) ScanEventGridTopics(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	if s.SubscriptionID == "" {
		// Defense-in-depth: the dispatcher's token-acquisition path
		// already guards against unconfigured scanners; the explicit
		// guard here keeps the standalone public API safe.
		return nil, nil
	}
	token, err := s.acquireAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure: %s: %w", ServiceIDEventGrid, err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.SubscriptionID
	}
	return s.scanEventGridForDispatcher(ctx, token, accountID)
}

// scanEventGridForDispatcher is the token-already-acquired body of
// ScanEventGridTopics. Factored out so the slice 6 chunk 1 two-way
// dispatcher (ScanEventSources) can share the token across the Service
// Bus and Event Grid surfaces without re-acquiring.
//
// Returns (rows, nil) on a successful topic-list walk — even when
// per-topic diagnostic-settings probes failed (those leave HasLogAxis
// false but still surface the row). Returns (nil, err) only when the
// subscription-wide topic list call itself failed — that is the
// dispatcher's "Event Grid surface failed" signal and the two-way
// partial-scan posture triggers off it.
func (s *Scanner) scanEventGridForDispatcher(ctx context.Context, accessToken, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	topics, listErr := s.listEventGridTopics(ctx, accessToken)
	if listErr != nil {
		return nil, fmt.Errorf("%s: %w", ServiceIDEventGrid, listErr)
	}
	if len(topics) == 0 {
		return nil, nil
	}
	out := make([]scanner.EventSourceInstanceSnapshot, 0, len(topics))
	for _, topic := range topics {
		hasLog, _ := s.probeEventGridDiagnostics(ctx, accessToken, topic.ID)
		// Per-topic probe failures leave HasLogAxis false but still
		// surface the row — see the function godoc. The dispatcher
		// has no partial-result accumulator at this layer (the slice
		// 1 chunk 3 result.Partial / FailedServices wiring lives in
		// the ScanServiceBus path; the slice 6 chunk 1 dispatcher
		// follows the two-way error-return contract instead).
		out = append(out, projectEventGridTopic(topic, hasLog, accountID))
	}
	return out, nil
}

// listEventGridTopics walks Microsoft.EventGrid/topics at subscription
// scope and follows nextLink for pagination. Returns the accumulated
// topic list on success or a wrapped *armCallError on any non-200 /
// transport failure — mirrors listServiceBusNamespaces from slice 1
// chunk 3.
func (s *Scanner) listEventGridTopics(ctx context.Context, accessToken string) ([]armEventGridTopic, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.EventGrid/topics?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		EventGridTopicsAPIVersion,
	)

	var out []armEventGridTopic
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armEventGridTopicListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("event grid topics list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// probeEventGridDiagnostics returns (hasLog, err) for an Event Grid
// topic's microsoft.insights/diagnosticSettings sub-resource.
// Detection per docs/proposals/event-source-tier-slice6.md §3:
//
//   - hasLog flips when ANY diagnostic setting routes to either an
//     Application Insights resource (properties.applicationInsights.
//     connectionString) OR a Log Analytics workspace (properties.
//     workspaceId). This is a stricter rule than the Service Bus
//     slice 1 chunk 3 "any setting at all" axis — slice 6's design
//     doc §3 names the App Insights OR Log Analytics destinations as
//     load-bearing per the eventgrid-diagnostics-enable
//     recommendation, so the axis is only satisfied when one of
//     those two destinations is wired.
//
// 404 → (false, nil) — canonical "no diagnostic settings configured"
// shape; not a partial failure. Mirrors the Service Bus pattern.
//
// Other call failures return the *armCallError so the caller's
// downstream classifier (informational only at the slice 6 chunk 1
// dispatcher layer) can dispatch on it. The dispatcher does NOT
// abort the topic row — the dispatcher pattern is "surface the
// inventory even when per-axis detection failed".
//
// Reuses the slice 1 chunk 3 armServiceBusDiagnosticSettingsResponse
// types verbatim — the microsoft.insights/diagnosticSettings sub-
// resource has the same JSON shape regardless of the parent
// resource provider, so duplicating the wire types would be drift
// risk for no benefit. The shared shape is documented on the
// armServiceBusDiagnosticSettingsResponse godoc.
func (s *Scanner) probeEventGridDiagnostics(ctx context.Context, accessToken, topicARMID string) (bool, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(topicARMID, "/")
	diagURL := fmt.Sprintf(
		"%s/%s/providers/microsoft.insights/diagnosticSettings?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		resourceID,
		EventGridDiagnosticSettingsAPIVersion,
	)

	body, callErr := s.doARMGet(ctx, accessToken, diagURL)
	if callErr != nil {
		var ace *armCallError
		if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, callErr
	}

	// Reuse the slice 1 chunk 3 Service Bus diagnostic-settings wire
	// types — the microsoft.insights/diagnosticSettings sub-resource
	// has the same JSON shape across Azure resource providers, so
	// duplicating the wire shape would invite drift between the two
	// surfaces' detection rules.
	var resp armServiceBusDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, &armCallError{Wrapped: fmt.Errorf("event grid diagnostic settings parse: %w", jerr)}
	}

	for _, ds := range resp.Value {
		if ds.Properties.WorkspaceID != "" ||
			ds.Properties.ApplicationInsights.ConnectionString != "" {
			return true, nil
		}
	}
	return false, nil
}

// projectEventGridTopic maps a (topic, hasLog) pair into an
// EventSourceInstanceSnapshot per the slice 6 chunk 1 contract:
// Provider="azure", Surface="eventgrid", SourceType="topic",
// ResourceARN=topic.ID (ARM id is the canonical handle).
//
// HasTraceAxis is computed inline from the topic's inputSchema per
// §3: only "CloudEventSchemaV1_0" satisfies. The Detail bag carries
// per-topic informational context (input schema, public network
// access, disable local auth, provisioning state) — the per-cloud
// Inventory tab renders these alongside the universal columns.
func projectEventGridTopic(topic armEventGridTopic, hasLog bool, accountID string) scanner.EventSourceInstanceSnapshot {
	hasTrace := topic.Properties.InputSchema == EventGridCloudEventSchemaV1

	detail := map[string]any{
		"source_type": eventGridSourceTypeTopic,
		"has_trace":   hasTrace,
		"has_log":     hasLog,
	}
	if topic.Properties.InputSchema != "" {
		detail["input_schema"] = topic.Properties.InputSchema
	}
	if topic.Properties.PublicNetworkAccess != "" {
		detail["public_network_access"] = topic.Properties.PublicNetworkAccess
	}
	if topic.Properties.DisableLocalAuth {
		detail["disable_local_auth"] = true
	}
	if topic.Properties.ProvisioningState != "" {
		detail["provisioning_state"] = topic.Properties.ProvisioningState
	}

	return scanner.EventSourceInstanceSnapshot{
		Provider:     azureProviderID,
		Surface:      EventGridSurface,
		AccountID:    accountID,
		Region:       topic.Location,
		ResourceName: topic.Name,
		ResourceARN:  topic.ID,
		SourceType:   eventGridSourceTypeTopic,
		HasTraceAxis: hasTrace,
		HasLogAxis:   hasLog,
		// Slice 6 chunk 1 does NOT compute the slice 2 propagation
		// axis (per-subscription filter rule inspection is slice 8+
		// per the design doc §2 non-goals). Defaults to false; the
		// chunk-1 detection cannot prove or disprove the axis, so
		// the recommendation surface for chunk-2 does not key off
		// it for Event Grid rows.
		HasPropagationConfig: false,
		Detail:               detail,
	}
}
