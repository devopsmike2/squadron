// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// armServiceBusNamespaceListResponse is the JSON shape returned by
// the subscription-wide Microsoft.ServiceBus/namespaces list call.
// Slice 1 chunk 3 (v0.89.101, #736 Stream 134).
type armServiceBusNamespaceListResponse struct {
	Value    []armServiceBusNamespace `json:"value"`
	NextLink string                   `json:"nextLink,omitempty"`
}

// armServiceBusNamespace is the bare JSON shape of a single
// Microsoft.ServiceBus/namespaces entry. The slice-1 chunk-3
// projection reads ID / Name / Location / SKU; other properties
// (createdAt, status, encryption envelope, private endpoint
// connections) are intentionally untyped — slice 2 may extend the
// projection if operator feedback warrants.
type armServiceBusNamespace struct {
	ID       string                           `json:"id"`
	Name     string                           `json:"name"`
	Location string                           `json:"location"`
	Tags     map[string]string                `json:"tags,omitempty"`
	SKU      armServiceBusNamespaceSKU        `json:"sku"`
	Type     string                           `json:"type,omitempty"`
	Props    armServiceBusNamespaceProperties `json:"properties,omitempty"`
}

// armServiceBusNamespaceSKU carries the SKU envelope. Name is
// "Basic" / "Standard" / "Premium"; surfaced in the snapshot's Detail
// bag so the Inventory tab can dim Basic-tier rows.
type armServiceBusNamespaceSKU struct {
	Name string `json:"name,omitempty"`
	Tier string `json:"tier,omitempty"`
}

// armServiceBusNamespaceProperties carries lifecycle metadata
// reserved for slice 2's expanded surfacing — slice 1 chunk 3 keys
// only on the diagnostic-settings sub-resource.
type armServiceBusNamespaceProperties struct {
	Status            string `json:"status,omitempty"`
	MinimumTLSVersion string `json:"minimumTlsVersion,omitempty"`
}

// armServiceBusDiagnosticSettingsResponse is the JSON shape returned
// by microsoft.insights/diagnosticSettings on a Service Bus namespace
// scope. An empty Value array maps to both axes false without partial
// failure; mirrors armLogicDiagnosticSettingsResponse from the
// orchestration tier (the diagnostic-settings sub-resource is the
// same shape across resource providers).
type armServiceBusDiagnosticSettingsResponse struct {
	Value []armServiceBusDiagnosticSetting `json:"value"`
}

// armServiceBusDiagnosticSetting is a single Diagnostic Setting on a
// Service Bus namespace. The detection rule inspects Properties:
// WorkspaceID (Log Analytics) and ApplicationInsights.ConnectionString
// (direct App Insights) flip HasTraceAxis. EventHubAuthRuleID and
// StorageAccountID flip HasLogAxis only — logging-only sinks per the
// Logic Apps Consumption-tier pattern.
type armServiceBusDiagnosticSetting struct {
	ID         string                                   `json:"id,omitempty"`
	Name       string                                   `json:"name,omitempty"`
	Properties armServiceBusDiagnosticSettingProperties `json:"properties"`
}

// armServiceBusDiagnosticSettingProperties carries the destination
// fields the detection rule reads. Shape mirrors
// armLogicDiagnosticSettingProperties from the orchestration tier so
// a future shared-helper extraction has a single canonical shape.
type armServiceBusDiagnosticSettingProperties struct {
	WorkspaceID         string                                        `json:"workspaceId,omitempty"`
	StorageAccountID    string                                        `json:"storageAccountId,omitempty"`
	EventHubAuthRuleID  string                                        `json:"eventHubAuthorizationRuleId,omitempty"`
	EventHubName        string                                        `json:"eventHubName,omitempty"`
	ApplicationInsights armServiceBusDiagnosticAppInsightsDestination `json:"applicationInsights,omitempty"`
	Logs                []armServiceBusDiagnosticLogCategory          `json:"logs,omitempty"`
	Metrics             []armServiceBusDiagnosticMetricCategory       `json:"metrics,omitempty"`
}

// armServiceBusDiagnosticAppInsightsDestination carries the App
// Insights connection-string envelope. A non-empty ConnectionString
// flips HasTraceAxis per §3.3.
type armServiceBusDiagnosticAppInsightsDestination struct {
	ConnectionString string `json:"connectionString,omitempty"`
}

// armServiceBusDiagnosticLogCategory / armServiceBusDiagnosticMetricCategory —
// per-category routing blocks; reserved for slice-2 per-category
// detection refinement (today the chunk-3 rule reads the destination
// fields only, not the per-category enablement bits).
type armServiceBusDiagnosticLogCategory struct {
	Category string `json:"category,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
}

type armServiceBusDiagnosticMetricCategory struct {
	Category string `json:"category,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
}

// Slice 2 chunk 3 (v0.89.106, #743 Stream 141) of the Event source
// tier arc — per-namespace authorizationRules surface inspection. The
// detection rule lives in inspectAuthorizationRules: a namespace
// counts as "propagation preserved" when at least one of its
// authorizationRules carries the Send right (Send is the minimum
// required for a publisher to attach ApplicationProperties — where
// traceparent rides on Service Bus messages). Send-less namespaces
// (Listen-only rules, or no rules at all) cannot have any publisher
// attach the W3C trace header per the SAS-policy rule the design doc
// §3.3 names.
//
// See docs/proposals/event-source-tier-slice2.md §3.3 for the
// detection logic and §11 acceptance tests 11-12.

// ServiceBusAuthorizationRule is the JSON shape of a single
// Microsoft.ServiceBus/namespaces/authorizationRules entry returned
// by the per-namespace authorizationRules list call. The slice 2
// chunk 3 detection rule reads Name (so per-rule notes can quote it
// back at the operator) and Properties.Rights (the actual rule
// permissions).
type ServiceBusAuthorizationRule struct {
	ID         string                                `json:"id,omitempty"`
	Name       string                                `json:"name"`
	Type       string                                `json:"type,omitempty"`
	Properties ServiceBusAuthorizationRuleProperties `json:"properties"`
}

// ServiceBusAuthorizationRuleProperties carries the per-rule Rights
// list. Azure documents three rights:
//
//   - "Listen" — receive messages from a queue / topic subscription.
//   - "Send"   — send messages to a queue / topic. The minimum right
//     required for a publisher to attach ApplicationProperties
//     (where traceparent rides on Service Bus messages).
//   - "Manage" — full administrative access; implies Listen + Send
//     plus rule / entity management.
//
// The slice 2 chunk 3 detection rule reads Send specifically — a
// rule with Manage also satisfies (Manage implies Send) but the
// chunk-3 simplification keeps the check direct against Send. Slice
// 3 may add Manage-implies-Send refinement if operators surface
// Manage-only rules in practice.
type ServiceBusAuthorizationRuleProperties struct {
	Rights []string `json:"rights"`
}

// ServiceBusAuthorizationRulesResponse is the JSON shape returned by
// the per-namespace authorizationRules list call. NextLink follows
// the standard ARM pagination convention — an empty / missing
// NextLink signals "no more pages".
type ServiceBusAuthorizationRulesResponse struct {
	Value    []ServiceBusAuthorizationRule `json:"value"`
	NextLink string                        `json:"nextLink,omitempty"`
}
