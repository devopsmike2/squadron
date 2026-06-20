// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package scanner defines the provider-agnostic discovery contract for
// Squadron's universal-observation arc. Each cloud provider implements
// the Scanner interface; the proposer pipeline (and the connector
// wizard's validation endpoint) consume the typed results.
//
// The interface is designed for multi-cloud from day one per the
// universal-discovery design doc — slice 1's only implementation is
// AWS (see internal/discovery/aws), but adding GCP / Azure / on-prem
// in later slices is a matter of implementing this contract, not
// reworking the substrate or the recommendation surface.
//
// Result and ValidationResult types are intentionally provider-typed at
// the top (Provider field on Result) but category-typed underneath —
// ComputeInstanceSnapshot covers ec2 / gce / azure vm / vmware vm;
// FunctionRuntimeSnapshot covers lambda / cloud functions / azure
// functions. The proposer reasons about categories, not provider-
// specific resource types.
package scanner

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// Scanner is the provider-agnostic discovery contract. Implementations
// live in internal/discovery/<provider> and are wired into the
// validate endpoint and the (future) scheduled scan engine.
//
// Sessions are in-memory only — implementations must not persist
// short-lived credentials, per the security architecture in the
// design doc.
type Scanner interface {
	// Provider names the cloud (or on-prem source) this scanner
	// targets. Used by call sites to dispatch the right scanner for
	// a given CloudConnection.
	Provider() credstore.Provider

	// Scan walks the connection's inventory across the supplied
	// regions and returns a typed snapshot. Partial scans (e.g.
	// throttling cut the walk short) are signaled via Result.Partial.
	Scan(ctx context.Context, conn *credstore.CloudConnection, regions []string) (*Result, error)

	// Validate runs the connector-wizard pre-commit checks: confirm
	// the assume-role works and that the configured services are
	// reachable in at least one region. Creates zero persistent
	// records — the call is safe to invoke from the wizard before
	// the operator has clicked Save.
	Validate(ctx context.Context, conn *credstore.CloudConnection) (*ValidationResult, error)
}

// Result is the typed snapshot a single scan produces. Mirrors the
// CloudDiscoveryContext shape from the design doc so the proposer
// pipeline can consume it without an intermediate transformer.
type Result struct {
	// ScanID identifies this scan in audit + recommendation events.
	// Implementations are expected to set a UUID at scan start.
	ScanID string `json:"scan_id"`

	// ScanStartedAt / ScanCompletedAt bracket the scan. Both are set
	// even when Partial is true.
	ScanStartedAt   time.Time `json:"scan_started_at"`
	ScanCompletedAt time.Time `json:"scan_completed_at"`

	// Provider mirrors Scanner.Provider() — denormalized here so the
	// Result is self-describing once it leaves the scanner package.
	Provider credstore.Provider `json:"provider"`

	// AccountID is the provider-native primary identifier of the
	// scanned connection (account_id / project_id / subscription_id /
	// site_id).
	AccountID string `json:"account_id"`

	// Regions is the list of regions actually walked. Slice 1 ships
	// single-entry slices; slice 3 will iterate.
	Regions []string `json:"regions"`

	// Compute is the EC2 / GCE / Azure VM / VMware VM inventory.
	Compute []ComputeInstanceSnapshot `json:"compute"`

	// Functions is the Lambda / Cloud Functions / Azure Functions
	// inventory.
	Functions []FunctionRuntimeSnapshot `json:"functions"`

	// InstrumentedCount sums Compute+Functions entries where OTel
	// presence was detected. UninstrumentedCount is the complement.
	// Both are denormalized so consumers don't need to recount.
	InstrumentedCount   int `json:"instrumented_count"`
	UninstrumentedCount int `json:"uninstrumented_count"`

	// Partial is true when the scan completed but did not cover the
	// full inventory (e.g. AWS rate-limited the walk). PartialReason
	// is the operator-visible explanation.
	Partial       bool   `json:"partial"`
	PartialReason string `json:"partial_reason,omitempty"`
}

// ComputeInstanceSnapshot is the category-typed view of a virtual
// machine. Provider-specific scanners populate this from EC2
// DescribeInstances / GCE Instances.list / Azure VMs.list.
type ComputeInstanceSnapshot struct {
	// ResourceID is the provider-native ID: EC2 instance id / GCE
	// instance name / Azure VM id / VMware vmref.
	ResourceID string `json:"resource_id"`

	// InstanceType is the provider-specific shape: m5.large /
	// n2-standard-4 / Standard_D4s_v3 / etc. Left as a raw string —
	// the proposer normalizes when reasoning about cost.
	InstanceType string `json:"instance_type"`

	// Tags is the provider's tag map normalized to string/string. EC2
	// tags arrive as a list of {Key,Value}; the scanner flattens
	// before populating this field.
	Tags map[string]string `json:"tags,omitempty"`

	// HasOTel is the scanner's best-effort detection of an OTel
	// agent on the instance. Slice 1 uses tag heuristics (any tag
	// key matching otel* case-insensitive). Slice 2 will add
	// process-list heuristics via SSM.
	HasOTel bool `json:"has_otel"`

	// OSFamily is "linux", "windows", or "unknown". Drives the
	// proposer's choice of installation snippet.
	OSFamily string `json:"os_family"`

	// Region is where the instance lives. Denormalized into the
	// snapshot so the proposer can reason about collector
	// colocation without referring back to the Result.
	Region string `json:"region"`
}

// FunctionRuntimeSnapshot is the category-typed view of a serverless
// function. Provider-specific scanners populate this from Lambda
// ListFunctions / Cloud Functions list / Azure Functions list.
type FunctionRuntimeSnapshot struct {
	// ResourceID is the provider-native identifier: Lambda ARN /
	// GCP function id / Azure function name. Stable across scans.
	ResourceID string `json:"resource_id"`

	// Name is the operator-readable name. Often the trailing
	// component of ResourceID but kept separate so the UI doesn't
	// have to parse ARNs.
	Name string `json:"name"`

	// Runtime is the provider-typed runtime string: "nodejs20",
	// "python3.11", "go1.21", etc. The proposer keys its
	// instrumentation guidance off this value.
	Runtime string `json:"runtime"`

	// HasOTelLayer is true when the function has an OTel layer (AWS),
	// extension (Azure), or lib import (GCP) attached. Slice 1
	// implements layer-ARN substring matching for AWS only.
	HasOTelLayer bool `json:"has_otel_layer"`

	// Region is where the function lives.
	Region string `json:"region"`
}

// ValidationResult is the response shape for Scanner.Validate. The
// connector wizard renders this directly as the "what just happened"
// confirmation panel — every field maps to a UI element.
type ValidationResult struct {
	// AssumeRoleOK is true when the scanner successfully assumed the
	// configured role (AWS), exchanged the workload identity (GCP),
	// or authenticated the principal (Azure). When false,
	// AssumeRoleErr carries the humanized explanation.
	AssumeRoleOK bool `json:"assume_role_ok"`

	// AssumeRoleErr is non-nil only when AssumeRoleOK is false.
	// Carries the humanized message the wizard renders verbatim.
	AssumeRoleErr *HumanizedError `json:"assume_role_err,omitempty"`

	// Preflight is the per-service "can we actually list things"
	// check. Slice 1 runs one PreflightCheck per (ec2, lambda) ×
	// (first region in the connection). Slice 3 will iterate
	// regions.
	Preflight []PreflightCheck `json:"preflight"`
}

// PreflightCheck is the per-service result of the connector wizard's
// test-before-commit step. SampleCount is intentionally tiny — the
// wizard is not running a real scan; it's just confirming
// permissions.
type PreflightCheck struct {
	// Service is the slice-1 service identifier: "ec2" or "lambda".
	// Future slices add "rds", "s3", "alb", and so on.
	Service string `json:"service"`

	// OK is true when the preflight call returned without an error.
	OK bool `json:"ok"`

	// SampleCount is the number of resources observed by the
	// preflight call (capped at 5 — this is a permissions probe,
	// not an inventory walk).
	SampleCount int `json:"sample_count"`

	// Err is non-nil only when OK is false. Carries the humanized
	// message naming the wizard step the operator should return to.
	Err *HumanizedError `json:"err,omitempty"`
}

// HumanizedError is the wizard-friendly error envelope. Every cloud
// provider's errors.go layer maps raw SDK errors into this shape;
// the UI renders Message verbatim and uses SuggestedStep to deep-link
// back to the wizard step the operator needs to fix.
type HumanizedError struct {
	// Code is the provider's raw error code. Surfaced so support
	// agents helping a stuck operator can pattern-match against the
	// provider's own documentation.
	Code string `json:"code"`

	// Message is the operator-visible explanation. Plain prose,
	// names the recoverable action.
	Message string `json:"message"`

	// SuggestedStep is the ConnectorWizard step ID the wizard should
	// scroll/navigate the operator back to. Common values:
	// "trust-policy", "role-arn", "validate".
	SuggestedStep string `json:"suggested_step"`

	// DocLink is an optional deep link into Squadron's docs (or the
	// provider's docs) for the operator who wants more context.
	DocLink string `json:"doc_link"`
}
