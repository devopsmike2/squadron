// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package azure implements scanner.Scanner against the Azure REST
// API for slice 1 of the Azure discovery arc (design doc:
// docs/proposals/azure-discovery-slice1.md, v0.89.50).
//
// Slice 1 scope: Virtual Machines only. The proposer drafts
// vm-otel-tag recommendations against VMs whose Tags don't include
// an otel* key (case-insensitive). Slice 2 will extend to Azure
// SQL; slice 3 to AKS; etc.
//
// Credentials: the scanner takes the Service Principal client_secret
// already unsealed by the caller, plus tenant_id + client_id +
// subscription_id, and constructs an Azure ARM client. The secret
// bytes are never logged, never embedded in errors, never returned
// in audit payloads.
//
// Library choice: this package speaks the Azure ARM REST API
// directly via net/http rather than pulling in
// github.com/Azure/azure-sdk-for-go/sdk/azidentity +
// .../sdk/resourcemanager/compute/armcompute. Rationale (parallel to
// the GCP slice 1 chunk 2 rationale in internal/discovery/gcp):
//   - Slice 1's mock surface is httptest-based; the Azure SDK's
//     transport overrides are more ceremony than the bare REST flow
//     needs, and a single httptest.Server can multiplex the OAuth
//     token endpoint plus the ARM VM list endpoint with no SDK
//     plumbing.
//   - The transitive dependency footprint is materially smaller (no
//     azidentity / azcore / armcompute pulled in just for one list
//     call against one Compute API surface). Slice 2's Azure SQL
//     scanner can revisit the trade-off if the SDK starts pulling
//     its weight.
//   - The slice-1 VM shape Squadron needs (Name, Location, Tags,
//     properties.hardwareProfile.vmSize,
//     properties.storageProfile.osDisk.osType) is a tiny subset of
//     the SDK's generated types; the bare JSON struct is easier to
//     reason about and review.
//
// The slice 1 scanner walks the subscription-wide VM listing
// endpoint
// (GET /subscriptions/{id}/providers/Microsoft.Compute/virtualMachines)
// rather than the per-resource-group endpoint, because a single
// call returns every VM across every resource group and slice 1's
// design doc §9 documents subscription-scope as the chosen walk.
package azure
