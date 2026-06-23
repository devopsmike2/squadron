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
