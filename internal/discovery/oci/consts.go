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

// ServiceIDDatabase is the slice-2 service identifier the scanner
// reports against Result.FailedServices when the Database walk
// (DB Systems + Autonomous Databases) produces a non-fatal error.
// Mirrors the cross-cloud pattern (cloudsql / azuresql) — the
// per-provider connection model carries the provider discriminator
// separately, so the identifier is unprefixed.
//
// See docs/proposals/database-tier-slice2.md §4.1 ("Result.
// FailedServices identifiers: OCI DB Systems / Autonomous Database
// scanner: ocidb").
const ServiceIDDatabase = "ocidb"

// databaseListAPIVersion pins the OCI Database /dbSystems and
// /autonomousDatabases list API path version. Single-sourced for
// the same reason as the compute / identity constants.
const databaseListAPIVersion = "20160918"

// dbProviderOCI is the Provider discriminator the scanner writes
// onto every database snapshot. Kept as a constant so future
// renames (or per-tenancy multi-region scanners) reuse the same
// string without scattering literal "oci" throughout the
// projection helpers.
const dbProviderOCI = "oci"

// dbEngineOracle is the canonical Engine string for every OCI
// database snapshot. Both DB Systems and Autonomous Databases
// run Oracle Database in slice 2 — OCI's MySQL HeatWave and the
// future PostgreSQL service have their own scanner extension
// arc.
const dbEngineOracle = "oracle"

// dbManagementEnabledStatus is the case-insensitive sentinel that
// flips DatabaseManagementEnabled to true on a snapshot. OCI's
// API surface uses "ENABLED" / "NOT_ENABLED"; the scanner is
// permissive on case to keep the rule resilient to future API
// version drift.
const dbManagementEnabledStatus = "ENABLED"

// dbAvailableLifecycleState is the OCI lifecycle state value the
// scanner treats as "this row has an observability surface to
// recommend on". Slice 2 skips non-AVAILABLE rows (TERMINATING,
// PROVISIONING, FAILED, etc.) — the proposer cannot usefully
// emit a Database Management enable plan step against a row that
// isn't running.
const dbAvailableLifecycleState = "AVAILABLE"
