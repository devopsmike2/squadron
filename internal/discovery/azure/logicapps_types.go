// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// armLogicWorkflowListResponse is the JSON shape returned by the
// subscription-wide Microsoft.Logic/workflows list call.
type armLogicWorkflowListResponse struct {
	Value    []armLogicWorkflow `json:"value"`
	NextLink string             `json:"nextLink,omitempty"`
}

// armLogicWorkflow is the bare JSON shape of a single
// Microsoft.Logic/workflows entry. The slice 1 chunk 3 projection
// reads ID / Name / Location / Properties.State; definition payloads
// are intentionally untyped.
type armLogicWorkflow struct {
	ID         string                     `json:"id"`
	Name       string                     `json:"name"`
	Location   string                     `json:"location"`
	Tags       map[string]string          `json:"tags,omitempty"`
	Properties armLogicWorkflowProperties `json:"properties"`
}

// armLogicWorkflowProperties carries the lifecycle State string
// ("Enabled" / "Disabled" / "Suspended" / ...). Surfaced in the
// snapshot's Detail bag so the Inventory tab can dim non-Enabled rows.
type armLogicWorkflowProperties struct {
	State string `json:"state,omitempty"`
}

// armLogicDiagnosticSettingsResponse is the JSON shape returned by
// microsoft.insights/diagnosticSettings on a workflow scope. An empty
// Value array maps to both axes false without partial failure.
type armLogicDiagnosticSettingsResponse struct {
	Value []armLogicDiagnosticSetting `json:"value"`
}

// armLogicDiagnosticSetting is a single Diagnostic Setting on a Logic
// Apps workflow. The detection rule inspects Properties.WorkspaceID
// (Log Analytics destination — flips both axes via the §3.3
// continuous-export pattern) and
// Properties.ApplicationInsights.ConnectionString (direct App Insights
// destination — flips HasTraceAxis).
type armLogicDiagnosticSetting struct {
	ID         string                              `json:"id,omitempty"`
	Name       string                              `json:"name,omitempty"`
	Properties armLogicDiagnosticSettingProperties `json:"properties"`
}

// armLogicDiagnosticSettingProperties carries the destination fields
// the detection rule reads. Logs / Metrics arrays are typed for future
// slice-2 per-category detection; slice 1 does not key on individual
// categories.
type armLogicDiagnosticSettingProperties struct {
	WorkspaceID         string                                   `json:"workspaceId,omitempty"`
	StorageAccountID    string                                   `json:"storageAccountId,omitempty"`
	EventHubAuthRuleID  string                                   `json:"eventHubAuthorizationRuleId,omitempty"`
	ApplicationInsights armLogicDiagnosticAppInsightsDestination `json:"applicationInsights,omitempty"`
	Logs                []armLogicDiagnosticLogCategory          `json:"logs,omitempty"`
	Metrics             []armLogicDiagnosticMetricCategory       `json:"metrics,omitempty"`
}

// armLogicDiagnosticAppInsightsDestination carries the App Insights
// connection-string envelope. A non-empty ConnectionString flips
// HasTraceAxis per §3.3.
type armLogicDiagnosticAppInsightsDestination struct {
	ConnectionString string `json:"connectionString,omitempty"`
}

// armLogicDiagnosticLogCategory / armLogicDiagnosticMetricCategory —
// per-category routing blocks; reserved for slice-2 per-category
// detection refinement.
type armLogicDiagnosticLogCategory struct {
	Category string `json:"category,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
}

type armLogicDiagnosticMetricCategory struct {
	Category string `json:"category,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
}
