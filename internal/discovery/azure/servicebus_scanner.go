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

	// ServiceBusAuthorizationRulesAPIVersion pins the ARM API version
	// used for the per-namespace authorizationRules list call. Slice
	// 2 chunk 3 of the Event source tier arc (v0.89.106, #743 Stream
	// 141) uses the same 2022-10-01-preview surface as the slice 1
	// namespaces list — the authorizationRules sub-resource is part
	// of the Microsoft.ServiceBus resource provider's preview
	// surface; switching to a stable GA version is reserved for
	// slice 3 once the surface promotes.
	ServiceBusAuthorizationRulesAPIVersion = "2022-10-01-preview"

	// ServiceBusRightSend / ServiceBusRightListen / ServiceBusRightManage
	// are the three Service Bus authorization rule rights. A rule
	// must include Send to permit publishing messages (which is
	// where ApplicationProperties / traceparent attaches). Slice 2
	// chunk 3 detection treats Send as the load-bearing right; the
	// other two are surfaced as informational context only.
	ServiceBusRightSend   = "Send"
	ServiceBusRightListen = "Listen"
	ServiceBusRightManage = "Manage"

	// servicebusPropagationNoteNoSendRule is the informational note
	// recorded against namespaces with no Send-capable authorization
	// rule. The proposer's chunk 5 reasoning text consumes this
	// string verbatim — keep the wording stable so the recommendation
	// envelope's reasoning field doesn't drift between releases.
	servicebusPropagationNoteNoSendRule = "namespace has no Send-capable authorization rule; publishers can't attach traceparent to ApplicationProperties"

	// servicebusPropagationNoteNoRules is the informational note
	// recorded against namespaces with zero authorization rules.
	// This is the canonical "RBAC-only namespace" shape — operators
	// who skip SAS rules entirely and rely on Azure RBAC for
	// publisher / consumer permissions hit this branch. The slice 2
	// chunk 3 heuristic cannot prove or disprove propagation in this
	// case (RBAC role-based property restrictions are a slice 3
	// concern per the design doc §3.3), so propagation defaults to
	// preserved with an informational note rather than emitting a
	// false-positive broken recommendation.
	servicebusPropagationNoteNoRules = "namespace has no SAS authorizationRules; relying on RBAC for publish/consume rights (slice 2 cannot inspect RBAC role property restrictions)"

	// servicebusPropagationNoteListAPIError is the informational
	// note recorded against namespaces whose authorizationRules list
	// call failed with a non-fatal error. Propagation defaults to
	// preserved per the "don't emit false-positive broken
	// recommendations against unknown configs" stance.
	servicebusPropagationNoteListAPIError = "namespace authorizationRules list call failed; propagation status unknown"
)

// ScanEventSources is the Azure scanner's event-source-tier entry point
// and satisfies the optional handlers.EventSourceDiscoveryScanner
// interface (see internal/api/handlers/discovery.go). Slice 1 chunk 3
// (v0.89.101) shipped Service Bus alone; slice 6 chunk 1 (v0.89.147,
// #787 Stream 185) extends the dispatcher to two-way (Service Bus +
// Event Grid) with a partial-scan posture that lets either surface
// fail independently without aborting the other — see docs/proposals/
// event-source-tier-slice6.md §5 (scanner contract) and §11
// acceptance tests 10-13.
//
// Scope semantics: scope.Regions is ignored — both Service Bus
// namespaces and Event Grid topics are subscription-scope on the ARM
// list endpoint. scope.AccountID overrides the per-row AccountID
// stamped on every snapshot; empty falls back to s.SubscriptionID.
//
// Partial-scan posture per docs/proposals/event-source-tier-slice6.md
// §5: when ONE of the surfaces fails (e.g. an IAM-gap on Service Bus
// while Event Grid's broader read-only RBAC succeeds, or vice versa)
// the REMAINING surface still surfaces. Only when BOTH surfaces fail
// does the dispatcher return a non-nil error wrapping every
// per-surface cause. The §12 threat model treats this as load-bearing:
// the dispatcher's both-directions partial-scan posture is pinned by
// acceptance tests 11 + 12 of the slice 6 design doc.
//
// IAM contract per docs/proposals/event-source-tier-slice1.md §12 +
// event-source-tier-slice6.md §12: the existing Reader role at
// subscription scope covers BOTH the Microsoft.ServiceBus/namespaces
// + Microsoft.EventGrid/topics list calls AND the shared
// microsoft.insights/diagnosticSettings sub-resource read across
// both surfaces. No IAM extension beyond what slice 1 provided.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	token, err := s.acquireAccessToken(ctx)
	if err != nil {
		// Token-endpoint failure is the ONE substrate-level hard
		// failure where neither surface is reachable; surface as a
		// non-nil error so the caller's audit emit path fires
		// scan_failed rather than the partial-scan posture below.
		// Tag the error under Service Bus for backward compat with
		// the slice 1 chunk 3 error-string contract — the
		// dispatcher's both-surfaces failure path (below) is the
		// only place a slice 6 chunk 1 error mentions Event Grid.
		return nil, fmt.Errorf("azure: %s: %w", ServiceIDServiceBus, err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.SubscriptionID
	}

	var all []scanner.EventSourceInstanceSnapshot

	namespaces, sbErr := s.scanServiceBusForDispatcher(ctx, token, accountID)
	if sbErr == nil {
		all = append(all, namespaces...)
	}

	topics, egErr := s.scanEventGridForDispatcher(ctx, token, accountID)
	if egErr == nil {
		all = append(all, topics...)
	}

	// Slice 8 chunk 1 (v0.89.153, #795 Stream 192) extends the
	// dispatcher to three-way (Service Bus + Event Grid + Event Hubs).
	// Event Hubs is Azure's analytics + telemetry intake primitive
	// (partitioned log), distinct from the messaging primitives.
	// See docs/proposals/event-source-tier-slice8.md §5.
	hubs, ehErr := s.scanEventHubsForDispatcher(ctx, token, accountID)
	if ehErr == nil {
		all = append(all, hubs...)
	}

	// Three-way partial-scan posture: only return an error when ALL
	// THREE surfaces failed. Any one- OR two-surface failure is
	// silenced at this layer so an IAM gap on one or two surfaces
	// doesn't drop the inventory the operator actually CAN see on
	// the remaining surface(s). Combinatorial single-failure paths
	// are pinned by slice 8 acceptance tests 11 + 12 + 13; the
	// two-of-three failure path is pinned by test 14; the
	// all-three-fail error-string contract by test 15.
	if sbErr != nil && egErr != nil && ehErr != nil {
		return all, fmt.Errorf("all azure event source surfaces failed: servicebus=%v eventgrid=%v eventhubs=%w", sbErr, egErr, ehErr)
	}

	return all, nil
}

// scanServiceBusForDispatcher is the slice 6 chunk 1 dispatcher-
// friendly wrapper around the slice 1 chunk 3 ScanServiceBus result-
// accumulator path. Returns (rows, nil) when the subscription-wide
// list call succeeded (even when per-namespace probes failed —
// those leave HasTraceAxis / HasLogAxis false but still surface the
// row); returns (nil, err) only when the list call itself failed,
// signaling the dispatcher that the Service Bus surface is offline
// and the partial-scan posture should trip if Event Grid also fails.
//
// ScanServiceBus (the slice 1 chunk 3 entry point) stays unchanged
// for backward compat with the slice 1 + slice 2 tests that call it
// directly with a *scanner.Result accumulator. This wrapper reuses
// ScanServiceBus's helpers without changing its signature — the
// list-failure detection rides on the same listServiceBusNamespaces
// helper the slice 1 walker already exercises.
func (s *Scanner) scanServiceBusForDispatcher(ctx context.Context, accessToken, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	namespaces, listErr := s.listServiceBusNamespaces(ctx, accessToken)
	if listErr != nil {
		return nil, fmt.Errorf("%s: %w", ServiceIDServiceBus, listErr)
	}
	if len(namespaces) == 0 {
		return nil, nil
	}
	out := make([]scanner.EventSourceInstanceSnapshot, 0, len(namespaces))
	for _, ns := range namespaces {
		hasTrace, hasLog, _ := s.probeServiceBusDiagnostics(ctx, accessToken, ns.ID)
		propPreserved, propNote, rulesErr := s.probeServiceBusPropagation(ctx, accessToken, ns.ID)
		if rulesErr != nil {
			// Mirrors the slice 1 chunk 3 ScanServiceBus posture: a
			// failing authorizationRules list defaults propagation
			// to preserved with the list-error informational note.
			propPreserved = true
			propNote = servicebusPropagationNoteListAPIError
		}
		out = append(out, projectServiceBusNamespace(ns, hasTrace, hasLog, propPreserved, propNote, accountID))
	}
	return out, nil
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
		propPreserved, propNote, rulesErr := s.probeServiceBusPropagation(ctx, accessToken, ns.ID)
		if rulesErr != nil {
			// Non-fatal: the operator should see the namespace
			// inventory even when the authorizationRules list call
			// fails. Propagation defaults to preserved per the
			// "don't emit false-positive broken recommendations
			// against unknown configs" stance; the informational
			// note carries the explanation forward to the side panel.
			recordPartialFailure(result, ServiceIDServiceBus, classifyServiceBusError(rulesErr))
			propPreserved = true
			propNote = servicebusPropagationNoteListAPIError
		}
		if probeErr != nil {
			recordPartialFailure(result, ServiceIDServiceBus, classifyServiceBusError(probeErr))
			result.EventSources = append(result.EventSources,
				projectServiceBusNamespace(ns, false, false, propPreserved, propNote, result.AccountID))
			continue
		}
		result.EventSources = append(result.EventSources,
			projectServiceBusNamespace(ns, hasTrace, hasLog, propPreserved, propNote, result.AccountID))
	}
}

// inspectAuthorizationRules implements the slice 2 chunk 3 detection
// rule. Returns (preserved, note). preserved is true when at least
// one rule has Send rights (the minimum required for a publisher to
// attach ApplicationProperties — where traceparent rides on Service
// Bus messages). note is empty when preserved or carries an
// informational string when not.
//
// Note: Azure RBAC role-based property restrictions are out of scope
// for slice 2 chunk 3 per the design doc §3.3 simplification — the
// chunk-3 heuristic reads authorizationRules only. A namespace with
// zero authorizationRules (RBAC-only) defaults to preserved with an
// informational note, NOT broken — the chunk-3 detection cannot
// prove the RBAC role lacks property restrictions, so the
// false-positive recommendation cost outweighs the missed-detection
// cost. Slice 3 may add direct RBAC role scanning per §3.3.
//
// Rule-level evaluation: a rule with Send → preserved. A rule with
// Manage also satisfies in practice (Manage implies Send + Listen)
// but the chunk-3 rule checks for Send directly — operators with
// Manage-only rules are vanishingly rare on Service Bus and the
// chunk-3 detection prefers a literal-string match over an
// implication tree. Slice 3 may broaden if operator feedback warrants.
func inspectAuthorizationRules(rules []ServiceBusAuthorizationRule) (bool, string) {
	if len(rules) == 0 {
		// Canonical "RBAC-only namespace" shape — operators who skip
		// SAS rules entirely. Default to preserved with informational
		// note per the no-false-positives stance.
		return true, servicebusPropagationNoteNoRules
	}
	hasSendCapable := false
	for _, rule := range rules {
		for _, right := range rule.Properties.Rights {
			if right == ServiceBusRightSend {
				hasSendCapable = true
				break
			}
		}
		if hasSendCapable {
			break
		}
	}
	if !hasSendCapable {
		return false, servicebusPropagationNoteNoSendRule
	}
	return true, ""
}

// probeServiceBusPropagation fetches the namespace's authorizationRules
// list and applies inspectAuthorizationRules. Returns (preserved,
// note, err). A nil err means the call succeeded; a non-nil err is
// the wrapped *armCallError from the underlying ARM GET (the caller
// records a partial failure and defaults propagation to preserved
// with the list-error note).
//
// 404 on the authorizationRules list is the canonical "no rules
// configured" shape — Service Bus returns an empty Value array for
// namespaces with zero rules in practice, but defensive 404 handling
// matches the diagnostic-settings probe pattern.
func (s *Scanner) probeServiceBusPropagation(ctx context.Context, accessToken, namespaceARMID string) (bool, string, error) {
	rules, err := s.listServiceBusAuthorizationRules(ctx, accessToken, namespaceARMID)
	if err != nil {
		var ace *armCallError
		if errors.As(err, &ace) && ace.StatusCode == http.StatusNotFound {
			// Canonical "no rules / RBAC-only namespace" shape.
			preserved, note := inspectAuthorizationRules(nil)
			return preserved, note, nil
		}
		return true, "", err
	}
	preserved, note := inspectAuthorizationRules(rules)
	return preserved, note, nil
}

// listServiceBusAuthorizationRules walks the per-namespace
// authorizationRules sub-resource and follows nextLink for
// pagination. Returns the accumulated rule list on success or a
// wrapped *armCallError on any non-200 / transport failure.
//
// URL shape:
//
//	GET {arm}/{namespaceARMID}/authorizationRules?api-version=...
//
// namespaceARMID arrives as the full ARM id with a leading slash;
// the path build mirrors probeServiceBusDiagnostics (TrimPrefix on
// the leading slash to avoid the double-slash that would otherwise
// land in the URL).
func (s *Scanner) listServiceBusAuthorizationRules(ctx context.Context, accessToken, namespaceARMID string) ([]ServiceBusAuthorizationRule, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(namespaceARMID, "/")
	pageURL := fmt.Sprintf(
		"%s/%s/authorizationRules?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		resourceID,
		ServiceBusAuthorizationRulesAPIVersion,
	)

	var out []ServiceBusAuthorizationRule
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page ServiceBusAuthorizationRulesResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("service bus authorization rules parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
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

// projectServiceBusNamespace maps a (namespace, hasTrace, hasLog,
// propagationPreserved, propagationNote) tuple into an
// EventSourceInstanceSnapshot per the slice-1 contract:
// Provider="azure", Surface="servicebus", SourceType="namespace",
// ResourceARN=namespace.ID (ARM id is the canonical handle).
//
// Slice 2 chunk 3 (v0.89.106, #743 Stream 141) adds the trailing
// (propagationPreserved, propagationNote) pair — the per-namespace
// authorizationRules detection result. propagationPreserved maps
// directly onto HasPropagationConfig; a non-empty propagationNote is
// appended to PropagationNotes (the slice-2 axis is a list-of-notes
// shape; chunk 3 emits at most one note per namespace).
func projectServiceBusNamespace(ns armServiceBusNamespace, hasTrace, hasLog, propagationPreserved bool, propagationNote, accountID string) scanner.EventSourceInstanceSnapshot {
	detail := map[string]any{
		"source_type": serviceBusSourceTypeNamespace,
		"has_trace":   hasTrace,
		"has_log":     hasLog,
	}
	if ns.SKU.Name != "" {
		detail["sku"] = ns.SKU.Name
	}
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:             azureProviderID,
		Surface:              serviceBusEventSourceSurface,
		AccountID:            accountID,
		Region:               ns.Location,
		ResourceName:         ns.Name,
		ResourceARN:          ns.ID,
		SourceType:           serviceBusSourceTypeNamespace,
		HasTraceAxis:         hasTrace,
		HasLogAxis:           hasLog,
		HasPropagationConfig: propagationPreserved,
		Detail:               detail,
	}
	if propagationNote != "" {
		snap.PropagationNotes = []string{propagationNote}
	}

	// DLQ configuration analysis slice 1 chunk 3 (v0.89.165, #807
	// Stream 204) — adds the three Azure Service Bus DLQ axis
	// Detail keys (has_dlq_queue_walk_available, dlq_retry_count,
	// dlq_retry_count_in_band) per
	// docs/proposals/dlq-configuration-analysis-slice1.md §3.2
	// honest framing (namespace-level scope; per-queue walk is a
	// future slice prerequisite).
	// ADDITIVE only — none of the slice-1 + slice-2 keys above are
	// modified here, so callers that have not yet adopted the DLQ
	// axis keys see byte-identical output to v0.89.164.
	applyServiceBusDLQDetail(&snap, ns)

	// Consumer lag detection slice 2 chunk 3 (v0.89.170, #812
	// Stream 209) — adds the four Azure Service Bus lag axis
	// Detail keys (lag_backlog_depth, lag_backlog_depth_high,
	// lag_consumer_silence_seconds, lag_consumer_silence_high) per
	// docs/proposals/consumer-lag-detection-slice2.md §3.4
	// (§3.2-inherited scanner-coverage-gap honest framing —
	// per-queue activeMessageCount sits at the unwalked
	// Microsoft.ServiceBus/namespaces/queues sub-resource). ADDITIVE
	// only — none of the slice-1 + slice-2 + slice-1-DLQ keys above
	// are modified here, so callers that have not yet adopted the
	// lag axis keys see byte-identical output to v0.89.169.
	applyServiceBusLagDetail(&snap, ns)

	// Poison-message rate analysis slice 3 chunk 3 (v0.89.175,
	// #817 Stream 214) — adds the two Azure Service Bus
	// poison-rate axis Detail keys (poison_rate_per_hour,
	// poison_rate_high_band) per
	// docs/proposals/poison-message-rate-slice3.md §3.3 honest
	// framing (also inherits §3.2 scanner-coverage-gap from
	// DLQ slice 1 chunk 3 + lag slice 2 chunk 3). ADDITIVE
	// only — none of the slice-1 + slice-2 + slice-1-DLQ +
	// slice-2-lag keys above are modified here, so callers
	// that have not yet adopted the poison-rate axis keys see
	// byte-identical output to v0.89.174.
	applyServiceBusPoisonRateDetail(&snap, ns)

	return snap
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
