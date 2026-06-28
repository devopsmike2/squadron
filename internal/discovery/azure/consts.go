// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// ServiceIDVirtualMachines is the slice-1 service identifier the
// scanner reports against Result.FailedServices when the Virtual
// Machines walk produces a non-fatal error. Mirrors the AWS scanner's
// bare service identifiers ("ec2", "rds", etc.) and the GCP slice-1
// scanner's "gce" — the connection model carries the provider
// discriminator separately, so the identifier is unprefixed.
//
// See docs/proposals/azure-discovery-slice1.md §9 ("Service
// identifier for partial failures: azurevm").
const ServiceIDVirtualMachines = "azurevm"

// OTelTagPrefix is the case-insensitive prefix the slice-1
// "instrumented" rule looks for on an Azure VM's tags. Mirrors the
// AWS EC2 / GCP GCE slice-1 single-axis tag heuristic — symmetry
// across providers makes the recommendation kinds parallel (see
// docs/proposals/azure-discovery-slice1.md §9). Slice 2 adds richer
// signals.
const OTelTagPrefix = "otel"

// armManagementEndpoint is the production Azure Resource Manager
// API base URL. Test scanners override via the armEndpoint field on
// Scanner; production code paths use this constant.
const armManagementEndpoint = "https://management.azure.com"

// armVMListAPIVersion pins the Microsoft.Compute/virtualMachines
// list-by-subscription API version. The 2024-07-01 surface returns
// the VM shape fields slice 1 needs (Tags, properties.hardwareProfile,
// properties.storageProfile.osDisk.osType) at stable JSON paths;
// future SDK revs can lift this without breaking the wire shape.
const armVMListAPIVersion = "2024-07-01"

// loginMicrosoftEndpoint is the production Azure AD token endpoint
// base URL. Test scanners override via the tokenEndpoint field on
// Scanner; production code paths use this constant.
const loginMicrosoftEndpoint = "https://login.microsoftonline.com"

// armScope is the OAuth2 scope the token request asks for. The
// .default suffix asks for every application permission already
// granted to the SP, which for slice 1 is Reader at the subscription
// scope.
const armScope = "https://management.azure.com/.default"

// ServiceIDAzureSQL is the slice-2 service identifier the scanner
// reports against Result.FailedServices when the Azure SQL walk
// (servers + databases + Diagnostic Settings) produces a non-fatal
// error. See docs/proposals/database-tier-slice2.md §4.1
// ("Result.FailedServices identifiers: Azure SQL scanner: azuresql").
const ServiceIDAzureSQL = "azuresql"

// armSQLAPIVersion pins the Microsoft.Sql/servers and databases
// list APIs. The 2023-08-01-preview surface returns the database
// shape fields slice 2 needs (sku.name, location, tags,
// properties.currentServiceObjectiveName) at stable JSON paths.
const armSQLAPIVersion = "2023-08-01-preview"

// armDiagSettingsAPIVersion pins the
// microsoft.insights/diagnosticSettings list API. 2021-05-01-preview
// returns the SQLInsights category routing shape slice 2 detects on.
const armDiagSettingsAPIVersion = "2021-05-01-preview"

// sqlInsightsCategory is the Diagnostic Settings log-category name
// the slice-2 detection rule keys on. Azure's category names are
// case-sensitive in the response body; the rule matches the exact
// string the Diagnostic Settings API publishes.
const sqlInsightsCategory = "SQLInsights"

// sqlMasterDatabase is the system database every SQL Server
// exposes. Squadron skips it during the walk — there is no
// operator-controllable observability surface on master, and
// emitting a recommendation against it would be noise.
const sqlMasterDatabase = "master"

// azureProviderID is the Provider discriminator the scanner writes
// into DatabaseInstanceSnapshot.Provider so the proposer can route
// to azsql-diag-enable rather than the default AWS branch. See
// internal/discovery/scanner/scanner.go::DatabaseInstanceSnapshot
// godoc for the empty=AWS backward-compat note.
const azureProviderID = "azure"

// azureSQLEngine is the Engine string the scanner writes into
// DatabaseInstanceSnapshot.Engine. Azure SQL is always the SQL
// Server engine — the database service does not multiplex engines
// the way GCP Cloud SQL does (postgres / mysql / sqlserver), so the
// scanner can hard-code this rather than reading a per-database
// field.
const azureSQLEngine = "sqlserver"

// ServiceIDAKS is the kubernetes-tier-slice-2 (chunk 3) service
// identifier the scanner reports against Result.FailedServices when
// the AKS managed-cluster walk produces a non-fatal error. See
// docs/proposals/kubernetes-tier-slice2.md §4.1
// ("Result.FailedServices identifiers: Azure AKS scanner: aks").
const ServiceIDAKS = "aks"

// armAKSAPIVersion pins the Microsoft.ContainerService/managedClusters
// list-by-subscription API version. 2024-09-01 returns both the
// legacy addonProfiles.omsagent shape AND the newer
// azureMonitorProfile.metrics / azureMonitorProfile.containerInsights
// shape at stable JSON paths — the slice-2 three-way disjunction
// detection rule reads all three.
const armAKSAPIVersion = "2024-09-01"

// aksOMSAgentAddonName is the legacy Container Insights addon
// profile key the three-way detection rule inspects under
// properties.addonProfiles. Older AKS clusters surface their
// observability state here; newer clusters use the
// azureMonitorProfile shape instead. The detection rule treats
// either as sufficient evidence for AzureMonitorEnabled=true.
const aksOMSAgentAddonName = "omsagent"

// aksRunningPowerState is the powerState.code value the scanner
// normalizes (alongside provisioningState=="Succeeded") into the
// canonical "RUNNING" Status string. AKS exposes two orthogonal
// lifecycle signals: provisioningState (the most recent
// management-plane operation outcome) and powerState (whether the
// cluster is started or stopped). A cluster is operator-actionable
// for observability recommendations only when BOTH are healthy;
// any other combination surfaces the raw provisioningState so the
// proposer's "decline to recommend against a non-RUNNING cluster"
// branch can fire.
const aksRunningPowerState = "Running"

// aksProvisioningSucceeded is the provisioningState value that
// pairs with powerState.code=="Running" to produce the canonical
// "RUNNING" Status. Other provisioningState values
// ("Creating" / "Updating" / "Deleting" / "Failed") map through
// verbatim — the proposer surfaces them raw so the Inventory tab
// can dim mid-lifecycle rows and the AI recommendation pipeline
// can skip them.
const aksProvisioningSucceeded = "Succeeded"

// aksStatusRunning is the canonical Status string the AKS walker
// writes when provisioningState=="Succeeded" AND
// powerState.code=="Running". Parallel to the AWS EKS "ACTIVE"
// and GCP GKE "RUNNING" conventions — every per-cloud cluster
// scanner emits its provider's natural healthy-status string into
// ClusterSnapshot.Status so the Inventory tab and the proposer's
// non-active-skip branch see provider-consistent values.
const aksStatusRunning = "RUNNING"

// Coverage-parity arc slice 3 — object-store + load-balancer tiers.
const (
	// ServiceIDAzureStorage is the object-store-tier service id.
	ServiceIDAzureStorage = "azurestorage"
	// ServiceIDAzureLB is the load-balancer-tier service id.
	ServiceIDAzureLB = "azurelb"
	// armStorageAPIVersion pins Microsoft.Storage/storageAccounts.
	armStorageAPIVersion = "2023-01-01"
	// armNetworkAPIVersion pins Microsoft.Network/loadBalancers.
	armNetworkAPIVersion = "2023-09-01"
)
