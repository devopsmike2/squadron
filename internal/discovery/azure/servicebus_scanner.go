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

// Event source tier slice 1 chunk 3 constants — see
// docs/proposals/event-source-tier-slice1.md §3.3 (Azure Service Bus
// detection axes) and §11 acceptance tests 7-9.
const (
	// ServiceBusNamespacesAPIVersion pins the ARM API version for the
	// Microsoft.ServiceBus/namespaces list call. The 2022-10-01-preview
	// surface is the stable long-term version slice 1 ships against.
	ServiceBusNamespacesAPIVersion = "2022-10-01-preview"

	// ServiceBusDiagnosticSettingsAPIVersion mirrors the Logic Apps
	// Consumption-tier API version from v0.89.96 chunk 3
	// (LogicAppsDiagnosticSettingsAPIVersion ==
	// armDiagSettingsAPIVersion). The microsoft.insights/
	// diagnosticSettings surface is the shared Azure observability-
	// routing primitive and every per-resource scanner pins the same
	// version.
	ServiceBusDiagnosticSettingsAPIVersion = "2021-05-01-preview"

	// ServiceIDServiceBus is the per-service identifier the scanner
	// reports against Result.FailedServices when the Service Bus walk
	// produces a non-fatal error.
	ServiceIDServiceBus = "servicebus"

	// serviceBusEventSourceSurface drives the proposer's recommendation-
	// kind prefix routing for Azure event sources: servicebus-* → Azure
	// (see docs/proposals/event-source-tier-slice1.md §8).
	serviceBusEventSourceSurface = "servicebus"

	// serviceBusSourceTypeNamespace is the SourceType discriminator
	// string for Service Bus namespace rows. Slice 1 chunk 3 surfaces
	// namespaces only; per-namespace queues / topics are slice-2.
	serviceBusSourceTypeNamespace = "namespace"
)

// ScanEventSources is the Azure scanner's event-source-tier entry point
// and satisfies the optional handlers.EventSourceDiscoveryScanner
// interface (see internal/api/handlers/discovery.go). Slice 1 chunk 3
// covers Microsoft.ServiceBus/namespaces only — mirrors the AWS
// scanner's ScanEventSources → ScanEventBridge layout from chunk 1 and
// the orchestration tier's ScanOrchestrations → ScanLogicApps layout
// from chunk 3 (v0.89.96).
//
// Scope semantics: scope.Regions is ignored — Service Bus namespaces are
// subscription-scope on the ARM list endpoint. scope.AccountID overrides
// the per-row AccountID stamped on every snapshot; empty falls back to
// s.SubscriptionID.
//
// IAM contract per docs/proposals/event-source-tier-slice1.md §12: the
// existing Reader role at subscription scope covers BOTH the
// Microsoft.ServiceBus/namespaces list AND the per-namespace
// microsoft.insights/diagnosticSettings sub-resource read.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	token, err := s.acquireAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure: %s: %w", ServiceIDServiceBus, err)
	}
	result := &scanner.Result{}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.SubscriptionID
	}
	result.AccountID = accountID
	s.ScanServiceBus(ctx, token, result)
	return result.EventSources, nil
}

// ScanServiceBus walks Microsoft.ServiceBus/namespaces at subscription
// scope and probes per-namespace microsoft.insights/diagnosticSettings,
// appending EventSourceInstanceSnapshot rows to result.EventSources.
// Slice 1 chunk 3 of the Event source tier arc (v0.89.101, #736 Stream
// 134) — see docs/proposals/event-source-tier-slice1.md §3.3 (detection
// rule) and §11 acceptance tests 7-9.
//
// Detection rule (mirrors the Logic Apps Consumption-tier detection
// from v0.89.96 chunk 3):
//
//   - HasLogAxis  ← ANY diagnostic setting present on the namespace
//     flips the axis (the "any setting at all" rule from the design
//     doc's §3.3 table). Storage / Event Hub destinations also flip
//     HasLogAxis because they constitute a structured-logging
//     destination wire-up even when they don't carry trace context.
//   - HasTraceAxis ← a diagnostic setting routing to App Insights
//     (properties.applicationInsights.connectionString) OR a Log
//     Analytics workspace (properties.workspaceId) flips the axis.
//     Storage and Event Hub destinations DO NOT flip HasTraceAxis —
//     they are logging-only sinks per the Logic Apps pattern; Event
//     Hub federation may carry traceparent properties on each message
//     but that is per-message inspection territory (slice 2).
//
// The two axes are surfaced independently; the OR-rule
// EventSourceInstanceSnapshot.IsInstrumented() collapses them at the
// proposer / count layer.
//
// 404 on the per-namespace diagnostic-settings GET is the canonical
// "no settings configured" shape — both axes stay false, no partial
// failure. This mirrors the Logic Apps Consumption tier path.
//
// Per-namespace diagnostic-settings call failures (non-404) record a
// partial under "servicebus" and STILL surface the namespace row with
// both axes false — the operator should see the inventory even when
// the per-axis detection failed. Mirrors the Logic Apps partial-
// failure posture from v0.89.96.
func (s *Scanner) ScanServiceBus(ctx context.Context, accessToken string, result *scanner.Result) {
	namespaces, listErr := s.listServiceBusNamespaces(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDServiceBus, classifyServiceBusError(listErr))
		return
	}
	if len(namespaces) == 0 {
		return
	}
	for _, ns := range namespaces {
		hasTrace, hasLog, probeErr := s.probeServiceBusDiagnostics(ctx, accessToken, ns.ID)
		if probeErr != nil {
			recordPartialFailure(result, ServiceIDServiceBus, classifyServiceBusError(probeErr))
			result.EventSources = append(result.EventSources,
				projectServiceBusNamespace(ns, false, false, result.AccountID))
			continue
		}
		result.EventSources = append(result.EventSources,
			projectServiceBusNamespace(ns, hasTrace, hasLog, result.AccountID))
	}
}

// listServiceBusNamespaces walks Microsoft.ServiceBus/namespaces at
// subscription scope and follows nextLink for pagination. Returns the
// accumulated namespace list on success or a wrapped *armCallError on
// any non-200 / transport failure — the caller's classifyServiceBusError
// maps the failure into the operator-visible PartialReason string.
func (s *Scanner) listServiceBusNamespaces(ctx context.Context, accessToken string) ([]armServiceBusNamespace, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.ServiceBus/namespaces?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		ServiceBusNamespacesAPIVersion,
	)

	var out []armServiceBusNamespace
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armServiceBusNamespaceListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("service bus namespaces list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// probeServiceBusDiagnostics returns (hasTrace, hasLog, err) for a
// namespace's diagnostic-settings sub-resource. Detection per §3.3:
//
//   - hasLog flips when ANY diagnostic setting is present.
//   - hasTrace flips when properties.workspaceId is populated (Log
//     Analytics workspace destination) OR
//     properties.applicationInsights.connectionString is populated
//     (direct App Insights destination).
//
// 404 → (false, false, nil) — canonical "no diagnostic settings
// configured" shape; not a partial failure.
//
// Event Hub and Storage destinations satisfy hasLog (any setting at
// all flips it) but DO NOT flip hasTrace — they are logging-only
// sinks per the Logic Apps Consumption-tier pattern.
func (s *Scanner) probeServiceBusDiagnostics(ctx context.Context, accessToken, namespaceARMID string) (hasTrace, hasLog bool, err error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(namespaceARMID, "/")
	diagURL := fmt.Sprintf(
		"%s/%s/providers/microsoft.insights/diagnosticSettings?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		resourceID,
		ServiceBusDiagnosticSettingsAPIVersion,
	)

	body, callErr := s.doARMGet(ctx, accessToken, diagURL)
	if callErr != nil {
		var ace *armCallError
		if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
			return false, false, nil
		}
		return false, false, callErr
	}

	var resp armServiceBusDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, false, &armCallError{Wrapped: fmt.Errorf("service bus diagnostic settings parse: %w", jerr)}
	}
	for _, ds := range resp.Value {
		// Any setting present satisfies §3.3 "if any diagnostic
		// setting is configured AT ALL" rule (HasLogAxis).
		hasLog = true
		// Trace axis: App Insights connection string (direct
		// destination) OR workspaceId (Log Analytics → App Insights
		// via continuous export). Storage and Event Hub destinations
		// stay logging-only.
		if ds.Properties.WorkspaceID != "" ||
			ds.Properties.ApplicationInsights.ConnectionString != "" {
			hasTrace = true
		}
	}
	return hasTrace, hasLog, nil
}

// projectServiceBusNamespace maps a (namespace, hasTrace, hasLog) triple
// into an EventSourceInstanceSnapshot per the slice-1 contract:
// Provider="azure", Surface="servicebus", SourceType="namespace",
// ResourceARN=namespace.ID (ARM id is the canonical handle).
func projectServiceBusNamespace(ns armServiceBusNamespace, hasTrace, hasLog bool, accountID string) scanner.EventSourceInstanceSnapshot {
	detail := map[string]any{
		"source_type": serviceBusSourceTypeNamespace,
		"has_trace":   hasTrace,
		"has_log":     hasLog,
	}
	if ns.SKU.Name != "" {
		detail["sku"] = ns.SKU.Name
	}
	return scanner.EventSourceInstanceSnapshot{
		Provider:     azureProviderID,
		Surface:      serviceBusEventSourceSurface,
		AccountID:    accountID,
		Region:       ns.Location,
		ResourceName: ns.Name,
		ResourceARN:  ns.ID,
		SourceType:   serviceBusSourceTypeNamespace,
		HasTraceAxis: hasTrace,
		HasLogAxis:   hasLog,
		Detail:       detail,
	}
}

// classifyServiceBusError maps a walk failure into the operator-visible
// PartialReason string under "servicebus". Mirrors the per-service
// classifier matrix from the orchestration / SQL / Compute tiers.
func classifyServiceBusError(err error) string {
	if err == nil {
		return ""
	}
	var ace *armCallError
	if errors.As(err, &ace) {
		if ace.IsNetwork {
			wrapped := ""
			if ace.Wrapped != nil {
				wrapped = ace.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", ServiceIDServiceBus, truncate(wrapped, 200))
		}
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDServiceBus)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", ServiceIDServiceBus)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", ServiceIDServiceBus)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", ServiceIDServiceBus)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: Service Bus walk failed (HTTP %d): %s", ServiceIDServiceBus, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDServiceBus, truncate(err.Error(), 200))
}
