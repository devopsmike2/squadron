// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// ServiceIDComputeEngine is the slice-1 service identifier the scanner
// reports against Result.FailedServices when the Compute Engine walk
// produces a non-fatal error. Mirrors the AWS scanner's bare service
// identifiers ("ec2", "rds", etc.) — the connection model (#667 design
// doc §4) carries the provider discriminator separately, so the
// identifier is unprefixed.
const ServiceIDComputeEngine = "gce"

// ServiceIDCloudSQL is the slice-2 (database-tier-slice2.md §4.1)
// service identifier the scanner reports against Result.FailedServices
// when the Cloud SQL walk produces a non-fatal error. Same unprefixed
// shape as ServiceIDComputeEngine.
const ServiceIDCloudSQL = "cloudsql"

// ServiceIDGKE is the kubernetes-tier-slice2.md §4.1 service identifier
// the scanner reports against Result.FailedServices when the GKE
// walk produces a non-fatal error. Same unprefixed shape as
// ServiceIDComputeEngine + ServiceIDCloudSQL.
const ServiceIDGKE = "gke"

// ContainerReadonlyScope is the OAuth scope the GKE Container API
// walk is authorized against. The google.golang.org/api/container/v1beta1
// package only exposes the platform-wide CloudPlatformScope as a
// constant — there is no targeted container.readonly constant in the
// generated package. We use the platform-wide read-only scope as the
// minimal-privilege fit (the scanner only reads cluster shapes; it
// never mutates).
//
// Why not the more-targeted "https://www.googleapis.com/auth/container.readonly"?
// That scope exists in Google's OAuth catalog but is NOT exposed by
// the v1beta1 client library as a named constant. The platform-wide
// read-only scope is the narrowest scope the generated client offers
// for read paths against the GKE control plane. The runbook documents
// roles/container.viewer as the IAM grant — the scope and the role
// are distinct axes (scope is the OAuth grant on the token; role is
// the project IAM grant on the SA principal), and the role is the
// least-privilege ask either way.
const ContainerReadonlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"

// ProviderGCP is the Provider discriminator value the scanner stamps
// on every DatabaseInstanceSnapshot it produces. The proposer reads
// this to route Cloud SQL rows to the cloudsql-pi-enable recommendation
// kind (see scanner.DatabaseInstanceSnapshot.Provider godoc).
const ProviderGCP = "gcp"

// OTelLabelPrefix is the case-insensitive prefix the slice-1
// "instrumented" rule looks for on a GCE instance's labels. Mirrors
// the AWS EC2 scanner's slice-1 single-axis tag heuristic — symmetry
// across providers makes the recommendation kinds parallel (see
// design doc §8). Slice 2 adds richer signals.
const OTelLabelPrefix = "otel"
