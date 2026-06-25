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

// Event source tier slice 8 chunk 1 (v0.89.153, #795 Stream 192) —
// Azure Event Hubs scanner. Adds Event Hubs as the third Azure event
// source surface alongside Service Bus (slice 1) and Event Grid
// (slice 6), bringing Azure to parity with AWS at 3 surfaces.
//
// See docs/proposals/event-source-tier-slice8.md.
//
// Two detection axes per design doc §3:
//   - Diagnostic settings (HasLogAxis): mirrors slice 1 + slice 6
//     pattern via the shared microsoft.insights/diagnosticSettings
//     sub-resource. Reuses the EventGridDiagnosticSettingsAPIVersion
//     constant from slice 6 and the
//     armServiceBusDiagnosticSettingsResponse JSON shape from slice 1.
//   - Capture (Detail["has_capture"]): Event-Hubs-specific axis. At
//     least one event hub in the namespace has
//     properties.captureDescription.enabled == true. The at-least-one
//     rule fires when ZERO hubs have Capture configured; operators
//     routinely have multiple hubs per namespace with different
//     durability requirements, so a blanket "every hub must have
//     Capture" rule would be too prescriptive.

const (
	// EventHubsNamespacesAPIVersion pins the ARM API version slice 8
	// uses for the Microsoft.EventHub/namespaces subscription-wide
	// list and per-namespace get calls. 2024-01-01 is the stable GA
	// surface that exposes the properties slice 8 reads
	// (isAutoInflateEnabled, zoneRedundant, disableLocalAuth, status).
	EventHubsNamespacesAPIVersion = "2024-01-01"

	// EventHubsHubsAPIVersion pins the ARM API version for the
	// per-namespace hubs list call (used by the Capture detection
	// axis). Matches EventHubsNamespacesAPIVersion — both surfaces
	// promoted to GA in the same Azure release.
	EventHubsHubsAPIVersion = "2024-01-01"

	// ServiceIDEventHubs is the per-service identifier the scanner
	// reports against Result.FailedServices when the Event Hubs walk
	// produces a non-fatal error. Mirrors ServiceIDServiceBus +
	// ServiceIDEventGrid — bare per-cloud-service identifier (the
	// provider discriminator is carried on the connection envelope).
	ServiceIDEventHubs = "eventhubs"

	// EventHubsSurface drives the proposer's recommendation-kind
	// prefix routing for Azure Event Hubs: eventhubs-* → Azure (see
	// docs/proposals/event-source-tier-slice8.md §8). The snapshot
	// Surface field carries this value verbatim.
	EventHubsSurface = "eventhubs"

	// eventHubsSourceTypeNamespace is the SourceType discriminator
	// string for Event Hubs Namespace rows. The per-cloud Inventory
	// tab keys off this in the per-surface column rendering. Slice 8
	// chunk 1 surfaces namespaces only — per-hub surfacing is slice 9+
	// candidate per design doc §13.
	eventHubsSourceTypeNamespace = "namespace"
)

// armEventHubsNamespaceListResponse is the JSON shape returned by the
// subscription-wide Microsoft.EventHub/namespaces list call. Mirrors
// armServiceBusNamespaceListResponse from slice 1 + armEventGridTopic
// ListResponse from slice 6 — NextLink follows the standard ARM
// pagination convention.
type armEventHubsNamespaceListResponse struct {
	Value    []armEventHubsNamespace `json:"value"`
	NextLink string                  `json:"nextLink,omitempty"`
}

// armEventHubsNamespace is the bare JSON shape of a single
// Microsoft.EventHub/namespaces entry. The slice 8 chunk 1 projection
// reads ID / Name / Location / Properties; other top-level fields
// (Type, Tags, SystemData, Sku) are intentionally untyped — slice 9+
// may extend the projection if operator feedback warrants.
type armEventHubsNamespace struct {
	ID         string                          `json:"id"`
	Name       string                          `json:"name"`
	Location   string                          `json:"location"`
	Tags       map[string]string               `json:"tags,omitempty"`
	Type       string                          `json:"type,omitempty"`
	Properties armEventHubsNamespaceProperties `json:"properties,omitempty"`
}

// armEventHubsNamespaceProperties carries the slice 8 chunk 1
// detection fields. None of these are load-bearing for the LOG axis
// (that's via the diagnostic-settings child); they all surface in the
// snapshot Detail bag as informational context per design doc §3.
type armEventHubsNamespaceProperties struct {
	Status                string `json:"status,omitempty"`
	IsAutoInflateEnabled  bool   `json:"isAutoInflateEnabled,omitempty"`
	ZoneRedundant         bool   `json:"zoneRedundant,omitempty"`
	DisableLocalAuth      bool   `json:"disableLocalAuth,omitempty"`
	KafkaEnabled          bool   `json:"kafkaEnabled,omitempty"`
	MaximumThroughputUnits int   `json:"maximumThroughputUnits,omitempty"`
}

// armEventHubsHubListResponse is the JSON shape returned by the
// per-namespace eventhubs list call (used by the Capture detection
// axis).
type armEventHubsHubListResponse struct {
	Value    []armEventHubsHub `json:"value"`
	NextLink string            `json:"nextLink,omitempty"`
}

// armEventHubsHub is the bare JSON shape of a single event hub
// (namespace child resource). Slice 8 chunk 1 reads ID + Name + the
// captureDescription block; the rest of the per-hub properties
// (partitionCount, messageRetentionInDays, status, etc.) are slice 9+
// territory if per-hub surfacing lands.
type armEventHubsHub struct {
	ID         string                    `json:"id"`
	Name       string                    `json:"name"`
	Properties armEventHubsHubProperties `json:"properties,omitempty"`
}

// armEventHubsHubProperties carries the per-hub fields slice 8 chunk
// 1 reads. CaptureDescription is the at-least-one-hub-enabled detection
// surface for the eventhubs-capture-enable recommendation kind.
type armEventHubsHubProperties struct {
	CaptureDescription armEventHubsCaptureDescription `json:"captureDescription,omitempty"`
}

// armEventHubsCaptureDescription is the per-hub Capture config block.
// Slice 8 chunk 1 only reads Enabled (the at-least-one-hub-enabled
// rule). The destination / interval / size fields the operator
// configures are surfaced via the iacpicker Terraform pattern in
// chunk 2; the scanner does not need them.
type armEventHubsCaptureDescription struct {
	Enabled bool `json:"enabled,omitempty"`
}

// ScanEventHubsNamespaces is the slice 8 chunk 1 entry point for the
// Azure Event Hubs surface. Lists Microsoft.EventHub/namespaces at
// subscription scope, probes per-namespace diagnostic settings AND
// the per-namespace hubs list (for Capture detection), and returns
// []EventSourceInstanceSnapshot rows with Surface="eventhubs".
//
// Scope semantics mirror ScanEventGridTopics: scope.Regions is
// ignored — Event Hubs namespaces are subscription-scope on the ARM
// list endpoint. scope.AccountID overrides the per-row AccountID
// stamped on every snapshot; empty falls back to s.SubscriptionID.
//
// Detection axes per docs/proposals/event-source-tier-slice8.md §3:
//
//   - HasLogAxis  ← the namespace has a microsoft.insights/
//     diagnosticSettings child that routes to either an Application
//     Insights resource (properties.applicationInsights.connectionString)
//     OR a Log Analytics workspace (properties.workspaceId). Mirrors
//     the slice 1 Service Bus + slice 6 Event Grid detection helpers.
//   - Detail["has_capture"] ← at least one event hub in the namespace
//     has properties.captureDescription.enabled == true. This is the
//     Event-Hubs-specific axis; the eventhubs-capture-enable
//     recommendation fires when the axis is false.
//
// HasTraceAxis stays false: Event Hubs has no schema-level trace
// context detection at the ARM API surface. Trace context propagation
// for Event Hubs flows via the AMQP application properties (or the
// Kafka headers when Kafka protocol is enabled), which is a
// client-side concern not detectable from the ARM API. Slice 9+ may
// add this if a substrate signal becomes available.
//
// IAM contract per docs/proposals/event-source-tier-slice8.md §12: the
// existing Reader role at subscription scope covers the
// Microsoft.EventHub/namespaces list, the per-namespace hubs list,
// AND the microsoft.insights/diagnosticSettings sub-resource read —
// no IAM extension beyond what slice 1 + slice 6 already provided.
//
// Returns (nil, nil) when the scanner has not been wired with a
// SubscriptionID — graceful posture matches the slice 6 chunk 1
// pattern.
func (s *Scanner) ScanEventHubsNamespaces(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	if s.SubscriptionID == "" {
		return nil, nil
	}
	token, err := s.acquireAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure: %s: %w", ServiceIDEventHubs, err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.SubscriptionID
	}
	return s.scanEventHubsForDispatcher(ctx, token, accountID)
}

// scanEventHubsForDispatcher is the token-already-acquired body of
// ScanEventHubsNamespaces. Factored out so the slice 8 chunk 1
// three-way dispatcher (ScanEventSources) can share the token across
// the Service Bus, Event Grid, and Event Hubs surfaces without
// re-acquiring.
//
// Returns (rows, nil) on a successful namespace-list walk — even when
// per-namespace diagnostic-settings probes OR per-namespace hubs-list
// calls failed (those leave the corresponding axis at false but still
// surface the namespace row). Returns (nil, err) only when the
// subscription-wide namespace list call itself failed — that is the
// dispatcher's "Event Hubs surface failed" signal and the three-way
// partial-scan posture triggers off it.
func (s *Scanner) scanEventHubsForDispatcher(ctx context.Context, accessToken, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	namespaces, listErr := s.listEventHubsNamespaces(ctx, accessToken)
	if listErr != nil {
		return nil, fmt.Errorf("%s: %w", ServiceIDEventHubs, listErr)
	}
	if len(namespaces) == 0 {
		return nil, nil
	}
	out := make([]scanner.EventSourceInstanceSnapshot, 0, len(namespaces))
	for _, ns := range namespaces {
		hasLog, _ := s.probeEventHubsDiagnostics(ctx, accessToken, ns.ID)
		hasCapture, _ := s.probeEventHubsCapture(ctx, accessToken, ns.ID)
		out = append(out, projectEventHubsNamespace(ns, hasLog, hasCapture, accountID))
	}
	return out, nil
}

// listEventHubsNamespaces walks Microsoft.EventHub/namespaces at
// subscription scope and follows nextLink for pagination. Returns the
// accumulated namespace list on success or a wrapped *armCallError on
// any non-200 / transport failure — mirrors listEventGridTopics.
func (s *Scanner) listEventHubsNamespaces(ctx context.Context, accessToken string) ([]armEventHubsNamespace, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.EventHub/namespaces?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		EventHubsNamespacesAPIVersion,
	)

	var out []armEventHubsNamespace
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armEventHubsNamespaceListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("event hubs namespaces list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// probeEventHubsDiagnostics returns (hasLog, err) for an Event Hubs
// namespace's microsoft.insights/diagnosticSettings sub-resource.
// Detection per docs/proposals/event-source-tier-slice8.md §3 — same
// rule as the slice 6 Event Grid detection: hasLog flips when ANY
// diagnostic setting routes to either an Application Insights resource
// (properties.applicationInsights.connectionString) OR a Log Analytics
// workspace (properties.workspaceId).
//
// 404 → (false, nil) — canonical "no diagnostic settings configured"
// shape; not a partial failure.
//
// Reuses the slice 1 chunk 3 armServiceBusDiagnosticSettingsResponse
// types verbatim — the microsoft.insights/diagnosticSettings
// sub-resource has the same JSON shape regardless of the parent
// resource provider.
func (s *Scanner) probeEventHubsDiagnostics(ctx context.Context, accessToken, namespaceARMID string) (bool, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(namespaceARMID, "/")
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

	var resp armServiceBusDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, &armCallError{Wrapped: fmt.Errorf("event hubs diagnostic settings parse: %w", jerr)}
	}

	for _, ds := range resp.Value {
		if ds.Properties.WorkspaceID != "" ||
			ds.Properties.ApplicationInsights.ConnectionString != "" {
			return true, nil
		}
	}
	return false, nil
}

// probeEventHubsCapture returns (hasCapture, err) for an Event Hubs
// namespace. Walks the per-namespace eventhubs list and returns true
// when AT LEAST ONE hub has properties.captureDescription.enabled ==
// true.
//
// Detection per docs/proposals/event-source-tier-slice8.md §3: the
// at-least-one-hub-enabled rule fires the eventhubs-capture-enable
// recommendation when ZERO hubs have Capture configured. Operators
// routinely have multiple hubs per namespace with different
// durability requirements; a blanket "every hub must have Capture"
// rule would be too prescriptive. The recommendation Terraform
// enables Capture on ONE hub (operator picks which during PR review).
//
// 404 on the hubs list → (false, nil) — canonical "namespace has no
// hubs yet" shape; the recommendation explicitly does NOT fire on
// empty namespaces per design doc §11 test 7 ("no hubs to audit").
//
// Any other hubs-list failure leaves hasCapture false but still
// surfaces the namespace row — same posture as the diagnostic
// settings probe. The operator sees the namespace inventory even
// when the per-axis detection failed.
func (s *Scanner) probeEventHubsCapture(ctx context.Context, accessToken, namespaceARMID string) (bool, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(namespaceARMID, "/")
	pageURL := fmt.Sprintf(
		"%s/%s/eventhubs?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		resourceID,
		EventHubsHubsAPIVersion,
	)

	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			var ace *armCallError
			if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
				return false, nil
			}
			return false, callErr
		}
		var page armEventHubsHubListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return false, &armCallError{Wrapped: fmt.Errorf("event hubs hubs list parse: %w", jerr)}
		}
		for _, hub := range page.Value {
			if hub.Properties.CaptureDescription.Enabled {
				return true, nil
			}
		}
		pageURL = page.NextLink
	}
	return false, nil
}

// projectEventHubsNamespace maps a (namespace, hasLog, hasCapture)
// triple into an EventSourceInstanceSnapshot per the slice 8 chunk 1
// contract: Provider="azure", Surface="eventhubs",
// SourceType="namespace", ResourceARN=namespace.ID.
//
// HasLogAxis is the diagnostic-settings axis. HasTraceAxis stays
// false — Event Hubs trace context propagation is client-side and
// not detectable from the ARM API. The Detail bag carries the
// slice 8 Capture axis (has_capture) plus per-namespace
// informational context (auto_inflate, zone_redundant,
// disable_local_auth, status, kafka_enabled).
func projectEventHubsNamespace(ns armEventHubsNamespace, hasLog, hasCapture bool, accountID string) scanner.EventSourceInstanceSnapshot {
	detail := map[string]any{
		"source_type":  eventHubsSourceTypeNamespace,
		"has_log":      hasLog,
		"has_capture":  hasCapture,
	}
	if ns.Properties.Status != "" {
		detail["status"] = ns.Properties.Status
	}
	if ns.Properties.IsAutoInflateEnabled {
		detail["auto_inflate"] = true
	}
	if ns.Properties.MaximumThroughputUnits > 0 {
		detail["maximum_throughput_units"] = ns.Properties.MaximumThroughputUnits
	}
	if ns.Properties.ZoneRedundant {
		detail["zone_redundant"] = true
	}
	if ns.Properties.DisableLocalAuth {
		detail["disable_local_auth"] = true
	}
	if ns.Properties.KafkaEnabled {
		detail["kafka_enabled"] = true
	}

	return scanner.EventSourceInstanceSnapshot{
		Provider:     azureProviderID,
		Surface:      EventHubsSurface,
		AccountID:    accountID,
		Region:       ns.Location,
		ResourceName: ns.Name,
		ResourceARN:  ns.ID,
		SourceType:   eventHubsSourceTypeNamespace,
		HasTraceAxis: false,
		HasLogAxis:   hasLog,
		// Slice 8 chunk 1 does NOT compute the slice 2 propagation
		// axis; per-partition trace context propagation is a
		// client-side concern not detectable from the ARM API.
		HasPropagationConfig: false,
		Detail:               detail,
	}
}
