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

// ServiceIDKubernetes is the slice-2 (kubernetes tier) service
// identifier the scanner reports against Result.FailedServices when
// the OKE cluster walk produces a non-fatal error. Mirrors the
// compute / database service identifiers ("ocicompute" / "ocidb");
// the per-provider connection model carries the provider
// discriminator separately, so the identifier is unprefixed
// ("oke").
//
// See docs/proposals/kubernetes-tier-slice2.md §4.1
// ("Result.FailedServices identifiers: OCI OKE scanner: oke").
const ServiceIDKubernetes = "oke"

// okeListAPIVersion pins the OCI Container Engine for Kubernetes
// /clusters list API path version. OCI versions live in the path
// (e.g. "/20180222/") not a query parameter; the constant lives
// here so the scanner path construction is single-sourced. OKE's
// API surface uses a different version date than Identity /
// Compute / Database — the constant keeps the per-surface version
// pin explicit.
const okeListAPIVersion = "20180222"

// clusterProviderOCI is the Provider discriminator the scanner
// writes onto every ClusterSnapshot row. Kept as a constant so
// future renames reuse the same string without scattering literal
// "oci" through the OKE projection helper.
const clusterProviderOCI = "oci"

// okeActiveLifecycleState is the OCI lifecycle state value the
// scanner treats as "this cluster has an observability surface to
// recommend on". Slice 2 skips non-ACTIVE rows (CREATING,
// DELETING, UPDATING, FAILED) — mid-create / mid-delete clusters
// can't usefully receive an Operations Insights plan step.
// Mirrors the AVAILABLE filter on the database surface; the
// per-surface enum value differs ("AVAILABLE" on DB Systems /
// Autonomous Databases; "ACTIVE" on OKE clusters) but the
// skip-non-active posture is identical.
const okeActiveLifecycleState = "ACTIVE"

// opsInsightsEnabledTagKey is the canonical lower-case form of the
// freeform tag key the slice-2 OKE detection rule looks for. The
// rule matches the key case-insensitively (operators may use
// "operations-insights-enabled", "Operations-Insights-Enabled", or
// any mixed-case variant); the constant carries the canonical
// lower-case shape so the comparison is centralized.
//
// See docs/proposals/kubernetes-tier-slice2.md §3.3 ("Detection
// rule: cluster is INSTRUMENTED if the cluster has a tag key
// matching operations-insights-enabled (case-insensitive) with
// value true").
const opsInsightsEnabledTagKey = "operations-insights-enabled"

// opsInsightsEnabledTagValue is the canonical lower-case form of
// the freeform tag value the slice-2 OKE detection rule looks for.
// Same case-insensitive convention as the key — operators may use
// "true", "TRUE", or "True" and the rule still fires. Slice 2
// design doc §11 test 7 pins this convention.
const opsInsightsEnabledTagValue = "true"
