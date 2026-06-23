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

// OTelLabelPrefix is the case-insensitive prefix the slice-1
// "instrumented" rule looks for on a GCE instance's labels. Mirrors
// the AWS EC2 scanner's slice-1 single-axis tag heuristic — symmetry
// across providers makes the recommendation kinds parallel (see
// design doc §8). Slice 2 adds richer signals.
const OTelLabelPrefix = "otel"
