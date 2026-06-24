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
