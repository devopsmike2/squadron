// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// armAKSListResponse is the JSON shape returned by the
// subscription-wide Microsoft.ContainerService/managedClusters list
// call. Only the fields the kubernetes-tier-slice-2 (chunk 3) walker
// actually reads are typed; the SDK exposes dozens of other managed
// cluster properties (network profile, agent pool profiles, identity
// blocks, etc.) that the Squadron proposer does not reason about
// today.
type armAKSListResponse struct {
	Value    []armAKSCluster `json:"value"`
	NextLink string          `json:"nextLink,omitempty"`
}

// armAKSCluster is the bare JSON shape of a single AKS managed
// cluster in the list response. The slice-2 projection reads
// ID, Name, Location, Tags, and four properties fields
// (kubernetesVersion, provisioningState, powerState.code,
// addonProfiles, azureMonitorProfile). Other top-level fields
// (sku, identity, type) are intentionally untyped — the
// recommendation surface does not key on them today.
type armAKSCluster struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags,omitempty"`
	Properties armAKSProperties  `json:"properties"`
}

// armAKSProperties is the bare JSON shape of the managed cluster
// properties sub-object. The detection rule (see
// docs/proposals/kubernetes-tier-slice2.md §3.2) keys on the
// three-way disjunction across AddonProfiles["omsagent"].Enabled,
// AzureMonitorProfile.Metrics.Enabled, and
// AzureMonitorProfile.ContainerInsights.Enabled. KubernetesVersion +
// ProvisioningState + PowerState drive the Status normalization
// per the chunk-3 brief.
type armAKSProperties struct {
	KubernetesVersion        string                     `json:"kubernetesVersion,omitempty"`
	CurrentKubernetesVersion string                     `json:"currentKubernetesVersion,omitempty"`
	ProvisioningState        string                     `json:"provisioningState,omitempty"`
	PowerState               *armAKSPowerState          `json:"powerState,omitempty"`
	AddonProfiles            map[string]armAKSAddon     `json:"addonProfiles,omitempty"`
	AzureMonitorProfile      *armAKSAzureMonitorProfile `json:"azureMonitorProfile,omitempty"`
}

// armAKSPowerState carries the cluster's started/stopped lifecycle
// signal. Pointer-typed so absent powerState blocks (the API can
// elide them on clusters mid-provisioning before the first start)
// project to a nil and the Status normalization can fall through
// to the raw provisioningState branch.
type armAKSPowerState struct {
	Code string `json:"code,omitempty"`
}

// armAKSAddon is the bare JSON shape of a single addon profile
// entry under properties.addonProfiles. The map key (e.g.
// "omsagent", "httpApplicationRouting") is the addon identifier;
// the Enabled flag drives the slice-2 detection rule for the
// legacy Container Insights addon. Config is intentionally
// untyped — the rule does not gate on the configured Log
// Analytics workspace id today.
type armAKSAddon struct {
	Enabled bool `json:"enabled"`
}

// armAKSAzureMonitorProfile is the newer (replacing the omsagent
// addon) observability profile sub-object. The slice-2 detection
// rule reads both nested flags independently — Metrics.Enabled
// (Managed Prometheus on AKS) and ContainerInsights.Enabled
// (Container Insights via the newer profile, replacing omsagent).
//
// All nested blocks are pointer-typed so an absent
// azureMonitorProfile block (older clusters) or an absent
// metrics/containerInsights sub-block (clusters that only enabled
// one) project to nil and the detection rule treats them as
// false rather than panicking on a missing field.
type armAKSAzureMonitorProfile struct {
	Metrics           *armAKSMonitorFlag `json:"metrics,omitempty"`
	ContainerInsights *armAKSMonitorFlag `json:"containerInsights,omitempty"`
}

// armAKSMonitorFlag is the common {enabled: bool} shape both
// metrics and containerInsights expose under
// azureMonitorProfile. Other fields (logAnalyticsWorkspaceResourceId
// for containerInsights, kubeStateMetrics for metrics) are
// intentionally untyped — the rule does not key on them.
type armAKSMonitorFlag struct {
	Enabled bool `json:"enabled"`
}
