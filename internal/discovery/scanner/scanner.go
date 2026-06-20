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
// functions; DatabaseInstanceSnapshot covers rds / cloud sql / azure
// sql. The proposer reasons about categories, not provider-specific
// resource types.
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

	// Databases is the RDS / Cloud SQL / Azure SQL inventory. Added in
	// slice 2 of the universal-observation arc — the proposer's
	// recommendation surface for databases reasons about Performance
	// Insights + Enhanced Monitoring enablement rather than an OTel
	// agent install (the latter is not how managed-database
	// observability works).
	Databases []DatabaseInstanceSnapshot `json:"databases"`

	// ObjectStores is the S3 / GCS / Azure Blob inventory. Added in
	// slice 3a of the universal-observation arc (v0.88.0). The
	// proposer's recommendation surface for object stores reasons
	// about Server Access Logging enablement — a single-axis,
	// operator-chosen-target lever; Squadron emits a recommendation
	// to enable logging to an operator-chosen bucket, but never
	// executes s3:PutBucketLogging.
	ObjectStores []ObjectStoreSnapshot `json:"object_stores"`

	// LoadBalancers is the ALB / NLB / GCLB / Azure LB inventory.
	// Added in slice 3a of the universal-observation arc (v0.88.0).
	// The proposer's recommendation surface for load balancers
	// reasons about Access Logs enablement, with a cross-reference
	// rule: when the inventory already contains S3 buckets, prefer
	// naming an existing bucket as the access-logs target so the
	// operator doesn't have to invent one. Squadron never executes
	// elasticloadbalancing:ModifyLoadBalancerAttributes.
	LoadBalancers []LoadBalancerSnapshot `json:"load_balancers"`

	// Clusters is the EKS / GKE / AKS managed-Kubernetes inventory.
	// Added in slice 3b of the universal-observation arc (v0.89.0).
	// The proposer's recommendation surface for clusters reasons
	// about a COMPOSITE rule: control plane logging (api + audit
	// minimum) AND an observability add-on (ADOT or CloudWatch
	// Observability) must BOTH be on. Single-axis recommendations
	// here would miss half the lever surface — operators with logs
	// on but no add-on still have no metrics/traces, and operators
	// with the add-on but no control-plane logging miss the
	// authentication / audit trail. Squadron emits enablement
	// recommendations as plan steps; never executes
	// eks:UpdateCluster or eks:CreateAddon.
	Clusters []ClusterSnapshot `json:"clusters"`

	// InstrumentedCount sums Compute+Functions+Databases+ObjectStores+
	// LoadBalancers+Clusters entries where observability presence
	// was detected.
	// UninstrumentedCount is the complement. Both are denormalized so
	// consumers don't need to recount.
	//
	// Per-category "instrumented" rules:
	//   - Compute: HasOTel == true
	//   - Functions: HasOTelLayer == true
	//   - Databases: PerformanceInsightsEnabled AND
	//     EnhancedMonitoringEnabled (both lights, the two-part rule)
	//   - ObjectStores: ServerAccessLoggingEnabled == true. (Slice 3a
	//     single-axis rule. RequestMetricsEnabled is informational only
	//     and does NOT gate the rule — surfaced for operator context.)
	//   - LoadBalancers: AccessLogsEnabled == true. (Slice 3a single-
	//     axis rule. AccessLogsS3Bucket is the operator-chosen target
	//     and stays informational.)
	//   - Clusters: control plane logging includes BOTH "api" AND
	//     "audit", AND at least one addon has Name=="adot" OR
	//     Name=="amazon-cloudwatch-observability" with
	//     Status=="ACTIVE". (Slice 3b composite rule — both axes
	//     required. Single-axis presence is informationally surfaced
	//     in the Inventory tab but does not count toward
	//     InstrumentedCount.)
	InstrumentedCount   int `json:"instrumented_count"`
	UninstrumentedCount int `json:"uninstrumented_count"`

	// Partial is true when the scan completed but did not cover the
	// full inventory (e.g. AWS rate-limited the walk). PartialReason
	// is the operator-visible explanation.
	Partial       bool   `json:"partial"`
	PartialReason string `json:"partial_reason,omitempty"`

	// FailedServices is the structured list of service identifiers
	// (e.g. "ec2", "lambda", "rds") whose walk produced a non-fatal
	// error during this scan. Mirrors the human-readable
	// PartialReason — audit consumers and the proposer's future
	// "learn from past scans" loop pattern-match against this field
	// rather than parsing the formatted string. Empty when Partial
	// is false.
	//
	// TODO(v0.87.4+): the AWS scanner currently OVERWRITES
	// PartialReason on each service failure rather than accumulating.
	// FailedServices is wired the same way for now (slice into a list
	// but slice 3 paths each call clear-then-append). When the
	// accumulator fix lands, both fields collect every failure.
	// Filed as a separate task.
	FailedServices []string `json:"failed_services,omitempty"`
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

// DatabaseInstanceSnapshot is the category-typed view of a managed
// database instance. Provider-specific scanners populate this from
// RDS DescribeDBInstances / Cloud SQL list / Azure SQL list. The
// proposer reasons about category-level levers (perf insights,
// enhanced monitoring, slow-query log shipping) rather than
// provider-specific feature names.
//
// Slice 2's "instrumented" rule for databases is two-part:
// PerformanceInsightsEnabled AND EnhancedMonitoringEnabled must both
// be true. The two levers are independent IAM permissions and
// independent ModifyDBInstance call shapes, so the proposer emits
// them as independent plan steps when either is missing — but the
// substrate's instrumented-count tally treats the row as covered only
// when both are on.
type DatabaseInstanceSnapshot struct {
	// ResourceID is the provider-native ID: RDS DB instance ARN /
	// Cloud SQL connection name / Azure SQL resource ID.
	ResourceID string `json:"resource_id"`

	// Engine is the provider-typed engine string: "postgres",
	// "mysql", "mariadb", "sqlserver", "oracle", "aurora-postgresql",
	// "aurora-mysql". The proposer keys its guidance off this.
	Engine string `json:"engine"`

	// EngineVersion is the provider-typed version, e.g. "15.4" for
	// postgres. Surfaced raw — the proposer only needs major version
	// class for instrumentation reasoning.
	EngineVersion string `json:"engine_version"`

	// InstanceClass is the provider-specific shape: db.r6g.large /
	// db-custom-2-7680 / GP_S_Gen5_2. Raw string — the proposer
	// normalizes when reasoning about cost.
	InstanceClass string `json:"instance_class"`

	// PerformanceInsightsEnabled signals AWS RDS Performance Insights
	// (or equivalent on other clouds). The proposer's primary RDS
	// lever — when false, recommend enabling.
	PerformanceInsightsEnabled bool `json:"performance_insights_enabled"`

	// EnhancedMonitoringEnabled signals AWS RDS Enhanced Monitoring
	// (per-second OS metrics via CloudWatch). The proposer's second
	// RDS lever.
	EnhancedMonitoringEnabled bool `json:"enhanced_monitoring_enabled"`

	// Region is where the instance lives.
	Region string `json:"region"`

	// Tags follows the same flattened shape as ComputeInstanceSnapshot.
	Tags map[string]string `json:"tags,omitempty"`
}

// ObjectStoreSnapshot is the category-typed view of an object-storage
// bucket. Provider-specific scanners populate this from S3
// ListBuckets+GetBucketLogging / GCS list / Azure Blob list. The
// proposer reasons about category-level levers (server access
// logging, request metrics) rather than provider-specific feature
// names.
//
// Slice 3a's "instrumented" rule for object stores is single-axis:
// ServerAccessLoggingEnabled must be true. RequestMetricsEnabled is
// informational only — surfaced so an operator can see request-rate
// observability state at a glance, but it does NOT gate the
// instrumented-count tally. The proposer prompt treats Server Access
// Logging as the single lever; when off, it recommends enabling
// (operator-chosen target bucket + prefix).
//
// Squadron does NOT execute s3:PutBucketLogging — discovery is
// strictly read-only; the operator runs the enablement Terraform
// through their own IaC pipeline. Same posture as RDS's PI / EM
// levers.
type ObjectStoreSnapshot struct {
	// ResourceID is the provider-native ID: S3 bucket name / GCS
	// bucket name / Azure Blob container path. Bucket names are
	// globally unique on AWS so the bare name suffices.
	ResourceID string `json:"resource_id"`

	// Region is where the bucket lives. S3 is technically a
	// global service for listing, but each bucket has a region
	// (returned by GetBucketLocation) that the proposer reasons
	// about for collector colocation.
	Region string `json:"region"`

	// ServerAccessLoggingEnabled signals AWS S3 Server Access
	// Logging (or equivalent on other clouds). The proposer's
	// primary S3 lever — when false, recommend enabling. Detection
	// reads s3:GetBucketLogging; the LoggingEnabled.TargetBucket
	// field being non-empty flips this to true.
	ServerAccessLoggingEnabled bool `json:"server_access_logging_enabled"`

	// RequestMetricsEnabled signals whether the bucket has S3
	// Request Metrics enabled (CloudWatch per-bucket request-rate
	// observability). Informational only — does NOT gate the
	// instrumented-count tally. Surfaced so the operator can see
	// request-rate observability state alongside the access-logging
	// lever.
	RequestMetricsEnabled bool `json:"request_metrics_enabled"`

	// Tags follows the same flattened shape as
	// ComputeInstanceSnapshot. Empty when GetBucketTagging returns
	// NoSuchTagSet.
	Tags map[string]string `json:"tags,omitempty"`
}

// LoadBalancerSnapshot is the category-typed view of a managed load
// balancer. Provider-specific scanners populate this from
// elasticloadbalancing:DescribeLoadBalancers (ALB / NLB / Gateway LB)
// / GCP list / Azure Load Balancer list. The proposer reasons about
// category-level levers (access logs to an object-storage target)
// rather than provider-specific feature names.
//
// Slice 3a's "instrumented" rule for load balancers is single-axis:
// AccessLogsEnabled must be true. AccessLogsS3Bucket is the
// operator-chosen target the proposer cross-references against the
// scan's ObjectStores list — recommending an ALB enable access logs
// to a bucket Squadron already sees in the inventory is the slice
// 3a forward-dependency payoff that justified pairing S3 and ALB in
// the same release.
//
// Squadron does NOT execute
// elasticloadbalancing:ModifyLoadBalancerAttributes — discovery is
// strictly read-only.
type LoadBalancerSnapshot struct {
	// ResourceID is the provider-native ID: ALB / NLB / Gateway LB
	// ARN / GCLB forwarding rule URL / Azure LB resource ID.
	ResourceID string `json:"resource_id"`

	// Name is the operator-readable name. Often the trailing
	// component of ResourceID but kept separate so the UI doesn't
	// have to parse ARNs.
	Name string `json:"name"`

	// Type is the load-balancer kind. On AWS one of "application",
	// "network", "gateway"; populated from
	// elasticloadbalancing:DescribeLoadBalancers' Type field.
	Type string `json:"type"`

	// Scheme is the load-balancer scheme: "internet-facing" or
	// "internal". Populated from DescribeLoadBalancers' Scheme
	// field. The proposer reasons about scheme when deciding
	// whether access logs are likely to be a compliance lever (an
	// internet-facing ALB without logs is a stronger
	// recommendation than an internal one).
	Scheme string `json:"scheme"`

	// AccessLogsEnabled signals whether the load balancer has
	// access logs enabled. The proposer's primary ALB lever — when
	// false, recommend enabling. Detection reads
	// DescribeLoadBalancerAttributes; the access_logs.s3.enabled
	// attribute flips this to true.
	AccessLogsEnabled bool `json:"access_logs_enabled"`

	// AccessLogsS3Bucket is the bucket the load balancer logs to,
	// when access logs are enabled. Populated from the
	// access_logs.s3.bucket attribute. Empty when access logs are
	// disabled or the attribute is unset. The proposer
	// cross-references this against the scan's ObjectStores list so
	// recommendations that name a target bucket can prefer one
	// Squadron already sees.
	AccessLogsS3Bucket string `json:"access_logs_s3_bucket,omitempty"`

	// Region is where the load balancer lives.
	Region string `json:"region"`

	// Tags follows the same flattened shape as the other category
	// snapshots.
	Tags map[string]string `json:"tags,omitempty"`
}

// ClusterSnapshot is the category-typed view of a managed Kubernetes
// cluster. Provider-specific scanners populate this from EKS
// DescribeCluster + ListAddons + ListNodegroups / GKE clusters.get
// / AKS managedClusters.get. The proposer reasons about
// category-level levers (control plane logging + observability
// add-on) rather than provider-specific feature names.
//
// Slice 3b's "instrumented" rule for clusters is COMPOSITE — two
// axes, both required:
//  1. Control plane logging on AT LEAST "api" AND "audit" types.
//     EKS supports five log types (api, audit, authenticator,
//     controllerManager, scheduler); the rule requires api +
//     audit because those two carry the load-bearing audit trail
//     Squadron's posture story depends on. Operators who turn on
//     authenticator / controllerManager / scheduler in addition
//     are MORE covered, not less, but the minimum is api+audit.
//  2. At least one addon with Name == "adot" (AWS Distro for
//     OpenTelemetry) OR Name == "amazon-cloudwatch-observability"
//     AND Status == "ACTIVE". DEGRADED / CREATE_FAILED / DELETING
//     addons do not count toward coverage even when present.
//
// Both axes must hold; either alone is insufficient. The proposer
// prompt teaches the same rule, so the operator-visible Inventory
// tab and the AI's reasoning denominate coverage identically.
//
// Squadron does NOT execute eks:UpdateCluster or eks:CreateAddon.
// The discovery role's permissions policy is strictly read-only
// (eks:ListClusters + eks:DescribeCluster + eks:ListAddons +
// eks:DescribeAddon + eks:ListNodegroups). The proposer surfaces
// enablement recommendations as plan steps; the operator runs the
// modify call through their own IaC tooling.
//
// Note on add-on naming: the observability add-on namespace AWS
// publishes evolves over time (today: "adot",
// "amazon-cloudwatch-observability"; new entrants are expected).
// This list lives in code so a future slice can extend it without
// touching the wire shape.
type ClusterSnapshot struct {
	// ResourceID is the provider-native ID: EKS cluster ARN / GKE
	// cluster path / AKS managed-cluster resource ID.
	ResourceID string `json:"resource_id"`

	// Name is the operator-readable cluster name. Often the
	// trailing component of ResourceID but kept separate so the UI
	// doesn't have to parse ARNs.
	Name string `json:"name"`

	// KubernetesVersion is the cluster's Kubernetes version string
	// (e.g. "1.29"). Surfaced raw — the proposer reads it for
	// per-version guidance (e.g. ADOT operator compatibility
	// floors).
	KubernetesVersion string `json:"kubernetes_version"`

	// Status is the provider-typed cluster status string:
	// "ACTIVE" / "CREATING" / "DELETING" / "FAILED" / "UPDATING".
	// Surfaced raw so the Inventory tab can dim non-ACTIVE rows
	// and the proposer can decline to recommend against a
	// non-ACTIVE cluster (mid-create / mid-delete clusters can't
	// usefully receive a plan step).
	Status string `json:"status"`

	// ControlPlaneLogging is the list of log types enabled for the
	// cluster's EKS control plane. AWS enum values:
	// "api" / "audit" / "authenticator" / "controllerManager" /
	// "scheduler". Empty slice means no log types are enabled.
	// The instrumented rule requires BOTH "api" AND "audit" be
	// present; other types are informationally surfaced.
	ControlPlaneLogging []string `json:"control_plane_logging"`

	// Addons is the per-cluster list of EKS managed add-ons. The
	// proposer reads the names to detect observability add-on
	// presence (ADOT or CloudWatch observability); the version +
	// status are denormalized so the Inventory tab can show
	// degradation state at a glance.
	Addons []ClusterAddon `json:"addons"`

	// NodegroupCount is the number of EKS managed node groups
	// attached to the cluster. Surfaced informationally — the
	// proposer does NOT emit per-nodegroup recommendations (the
	// cluster-level lever is the right scope for OTel coverage).
	NodegroupCount int `json:"nodegroup_count"`

	// FargateProfileCount is the number of EKS Fargate profiles
	// attached to the cluster. Surfaced informationally for the
	// same reason as NodegroupCount.
	FargateProfileCount int `json:"fargate_profile_count"`

	// Region is where the cluster lives.
	Region string `json:"region"`

	// Tags follows the same flattened shape as the other category
	// snapshots.
	Tags map[string]string `json:"tags,omitempty"`
}

// ClusterAddon is a single EKS managed add-on attached to a
// ClusterSnapshot. The instrumented rule reads Name + Status —
// Name identifies whether the add-on is an observability one
// (adot / amazon-cloudwatch-observability), Status whether it's
// actually running (ACTIVE) or in a degraded state (DEGRADED /
// CREATE_FAILED / DELETING — none of which count toward
// coverage).
type ClusterAddon struct {
	// Name is the AWS add-on name. Observability names recognized
	// by the instrumented rule today:
	//   - "adot" (AWS Distro for OpenTelemetry operator)
	//   - "amazon-cloudwatch-observability" (CloudWatch agent
	//     + Container Insights with enhanced observability)
	// Other names (aws-ebs-csi-driver, vpc-cni, coredns,
	// kube-proxy, etc.) are inventoried but do NOT flip the
	// instrumented bit.
	Name string `json:"name"`

	// Version is the add-on version (AWS returns this as a
	// semver-like string, e.g. "v0.92.0-eksbuild.1"). Surfaced
	// raw — the proposer doesn't reason about it today, but the
	// Inventory tab renders it as informational context.
	Version string `json:"version"`

	// Status is the AWS add-on status enum value. Values:
	// "CREATING" / "ACTIVE" / "CREATE_FAILED" / "UPDATING" /
	// "DELETING" / "DELETE_FAILED" / "DEGRADED" / "UPDATE_FAILED".
	// Only "ACTIVE" counts toward the observability coverage
	// rule — DEGRADED / CREATE_FAILED / DELETING are surfaced but
	// not counted.
	Status string `json:"status"`
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
	// Service is the per-service identifier: "ec2", "lambda", "rds",
	// "s3", "alb", "eks". Slice 2 added "rds"; slice 3a (v0.88.0)
	// added "s3" and "alb"; slice 3b (v0.89.0) added "eks"; future
	// slices add more.
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
