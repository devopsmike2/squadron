// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

// ServiceIDCompute is the slice-1 service identifier the scanner
// reports against Result.FailedServices when the Compute Instance
// walk produces a non-fatal error. Mirrors the AWS scanner's bare
// service identifiers ("ec2", "rds", etc.), the GCP slice-1
// scanner's "gce", and the Azure scanner's "azurevm" — the
// connection model carries the provider discriminator separately,
// so the identifier is unprefixed.
//
// See docs/proposals/oci-discovery-slice1.md §9 ("Service
// identifier for partial failures: ocicompute").
const ServiceIDCompute = "ocicompute"

// OTelTagPrefix is the case-insensitive prefix the slice-1
// "instrumented" rule looks for on an OCI Compute Instance's tags.
// Mirrors the AWS EC2 / GCP GCE / Azure VM slice-1 single-axis tag
// heuristic — symmetry across providers makes the recommendation
// kinds parallel (see docs/proposals/oci-discovery-slice1.md §9).
// Slice 2 adds richer signals.
const OTelTagPrefix = "otel"

// computeListAPIVersion pins the OCI Compute /instances list API
// path version. OCI versions live in the path (e.g. "/20160918/")
// not a query parameter; the constant lives here so the scanner
// path construction is single-sourced.
const computeListAPIVersion = "20160918"

// identityListAPIVersion pins the OCI Identity /compartments list
// API path version. Single-sourced for the same reason.
const identityListAPIVersion = "20160918"
