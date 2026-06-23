// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// armWebSiteListResponse is the JSON shape returned by the
// subscription-wide Microsoft.Web/sites list call. Only the fields
// the serverless-tier slice 1 chunk 3 walker actually reads are
// typed; the Microsoft.Web API exposes dozens of other site
// properties (hostNames, sslStates, identity, virtualNetworkSubnetId,
// etc.) that the Squadron proposer does not reason about today.
type armWebSiteListResponse struct {
	Value    []armWebSite `json:"value"`
	NextLink string       `json:"nextLink,omitempty"`
}

// armWebSite is the bare JSON shape of a single Microsoft.Web/sites
// entry in the list response. The slice 1 chunk 3 projection reads
// ID, Name, Location, Kind, and the nested SiteConfig fields the
// runtime normalization keys on.
//
// Kind is the load-bearing discriminator distinguishing Function
// Apps ("functionapp" / "functionapp,linux" /
// "functionapp,workflowapp" / "functionapp,linux,container") from
// regular Web Apps ("app" / "app,linux"). isFunctionApp uses a
// prefix match on the canonical "functionapp" string.
//
// Properties is pointer-typed so absent properties blocks (the API
// can elide them on partially-populated responses) project to a
// nil and the runtime extraction falls through to empty without
// panicking on a missing field.
type armWebSite struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Location   string             `json:"location"`
	Kind       string             `json:"kind"`
	Tags       map[string]string  `json:"tags,omitempty"`
	Properties *armWebSiteProps   `json:"properties,omitempty"`
}

// armWebSiteProps is the bare JSON shape of the Microsoft.Web/sites
// properties sub-object. The slice 1 chunk 3 path reads only the
// nested SiteConfig (for runtime detection); state, defaultHostName,
// reserved (the Linux discriminator), etc. are intentionally
// untyped — the rule does not key on them today.
type armWebSiteProps struct {
	SiteConfig *armWebSiteConfig `json:"siteConfig,omitempty"`
}

// armWebSiteConfig is the per-site runtime configuration. The two
// load-bearing fields for slice 1 chunk 3 are LinuxFxVersion (the
// "Family|Version" identifier for Linux Function Apps, e.g.
// "Python|3.11" / "Node|18" / "DotNet|6.0" / "DOTNET-ISOLATED|6.0")
// and WindowsFxVersion (the rare Windows-side analog for container
// deployments, e.g. "dotnet:6.0"). Older Windows Function Apps
// leave both blank and rely on the FUNCTIONS_WORKER_RUNTIME
// app_setting instead; the projection falls through to that
// fallback when both Fx fields are empty.
type armWebSiteConfig struct {
	LinuxFxVersion   string `json:"linuxFxVersion,omitempty"`
	WindowsFxVersion string `json:"windowsFxVersion,omitempty"`
}

// armAppSettingsResponse is the JSON shape returned by the
// Microsoft.Web/sites/<site>/config/appsettings/list POST. The
// settings live under properties as a string→string map (Azure
// publishes the canonical app_settings as plain strings; secret
// references via Key Vault are an Azure-side syntactic indirection
// the scanner does not unwrap — the value-presence detection rule
// honors the operator's intent as configured).
//
// Properties is pointer-typed so a defensive nil-check in the
// caller protects against an unexpected empty body shape — the
// rule's "settings_unread" partial-failure branch fires cleanly
// rather than panicking.
type armAppSettingsResponse struct {
	Properties map[string]string `json:"properties,omitempty"`
}
