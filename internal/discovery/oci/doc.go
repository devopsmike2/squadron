// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package oci implements scanner.Scanner against the OCI REST API
// for slice 1 of the OCI discovery arc (design doc:
// docs/proposals/oci-discovery-slice1.md, v0.89.55).
//
// Slice 1 scope: Compute Instances only. Recommendation kind:
// compute-otel-tag. Mirrors AWS/GCP/Azure slice 1 patterns.
//
// Authentication: manual OCI HTTP Signatures (RSA-SHA256) per
// the public spec. No oci-go-sdk dependency — keeps the slice 1
// dependency footprint minimal. Same posture as the GCP and
// Azure scanners' manual-REST approach.
package oci
