// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// armSQLServerListResponse is the JSON shape returned by the
// subscription-wide Microsoft.Sql/servers list call. Only the fields
// the slice-2 Azure SQL walker actually reads are typed; the SDK
// exposes dozens of other server properties (administratorLogin,
// fully qualified domain name, public network access, etc.) that the
// Squadron proposer does not reason about today.
type armSQLServerListResponse struct {
	Value    []armSQLServer `json:"value"`
	NextLink string         `json:"nextLink,omitempty"`
}

// armSQLServer is the bare JSON shape of a single SQL Server in the
// list response. ID carries the full ARM resource path (the walker
// extracts the resource group via parseRGFromARMID before issuing
// the per-server database list call).
type armSQLServer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	// Tags are read for completeness but slice 2 surfaces tags via
	// the per-database snapshot, not the per-server one.
	Tags map[string]string `json:"tags,omitempty"`
}

// armSQLDatabaseListResponse is the JSON shape returned by the
// per-server Microsoft.Sql/servers/{server}/databases list call.
type armSQLDatabaseListResponse struct {
	Value    []armSQLDatabase `json:"value"`
	NextLink string           `json:"nextLink,omitempty"`
}

// armSQLDatabase is the bare JSON shape of a single Azure SQL
// database. The slice-2 projection reads Name, Location, Tags, the
// sku.name shape (e.g. "GP_S_Gen5_2"), and the
// properties.currentServiceObjectiveName field (engine version /
// service tier signal).
type armSQLDatabase struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Location   string             `json:"location"`
	Tags       map[string]string  `json:"tags,omitempty"`
	Sku        armSQLSku          `json:"sku"`
	Properties armSQLDatabaseProp `json:"properties"`
}

// armSQLSku carries the database SKU shape — populated for both
// vCore and DTU-based databases. The Name field is the
// operator-readable SKU shorthand (e.g. "GP_S_Gen5_2",
// "Standard_S2", "BC_Gen5_4"); the projection writes this verbatim
// into DatabaseInstanceSnapshot.InstanceClass.
type armSQLSku struct {
	Name string `json:"name"`
	Tier string `json:"tier,omitempty"`
}

// armSQLDatabaseProp carries the database property fields the walker
// reads. CurrentServiceObjectiveName is the canonical service-tier
// signal Squadron projects into EngineVersion (more diagnostic than
// the SKU's tier alone — a "GP_S_Gen5_2" SKU has a service objective
// that may be "GP_S_Gen5_2" when the operator hasn't overridden it,
// but reflects scaling state precisely when they have).
type armSQLDatabaseProp struct {
	CurrentServiceObjectiveName string `json:"currentServiceObjectiveName,omitempty"`
	Status                      string `json:"status,omitempty"`
}

// armDiagnosticSettingsResponse is the JSON shape returned by
// microsoft.insights/diagnosticSettings on a database scope. An
// empty Value array (the API's "no settings" representation) maps
// to SQLInsightsDiagEnabled=false without surfacing a partial
// failure.
type armDiagnosticSettingsResponse struct {
	Value []armDiagnosticSetting `json:"value"`
}

// armDiagnosticSetting is a single Diagnostic Setting resource. The
// slice-2 detection rule inspects Properties.Logs for a SQLInsights
// category with Enabled=true; the destination (Log Analytics,
// Storage, Event Hub) is intentionally not gated — the rule is
// "SQLInsights routed to ANY destination flips the flag".
type armDiagnosticSetting struct {
	ID         string                   `json:"id,omitempty"`
	Name       string                   `json:"name,omitempty"`
	Properties armDiagnosticSettingProp `json:"properties"`
}

type armDiagnosticSettingProp struct {
	Logs []armDiagnosticLog `json:"logs,omitempty"`
}

// armDiagnosticLog is a single category routing block within a
// Diagnostic Setting. Category names are operator-facing strings
// ("SQLInsights", "AutomaticTuning", "QueryStoreRuntimeStatistics",
// "Errors", etc.); the slice-2 rule matches "SQLInsights" exactly
// (Azure's category names are case-sensitive in this response).
type armDiagnosticLog struct {
	Category string `json:"category"`
	Enabled  bool   `json:"enabled"`
}
