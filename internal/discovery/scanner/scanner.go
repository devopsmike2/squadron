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
	"strings"
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

	// DynamoDBTables is the DynamoDB (and future Cosmos DB / Cloud
	// Bigtable) inventory. Added in slice 4 of the
	// universal-observation arc (v0.89.6). The proposer's
	// recommendation surface for DynamoDB reasons about a
	// SINGLE-axis rule: ContributorInsightsStatus must be
	// "ENABLED". This is a deliberate downgrade from EKS slice 3b's
	// composite rule — DynamoDB has exactly one cloud-API-visible
	// observability signal per table that the operator must
	// explicitly enable; pretending the rule is composite would
	// either invent a fake second axis or pull in unrelated
	// operational signals (PITR, DAX presence) that aren't actually
	// observability. Squadron emits enablement recommendations as
	// plan steps; never executes
	// dynamodb:UpdateContributorInsights.
	DynamoDBTables []DynamoDBTableSnapshot `json:"dynamodb_tables"`

	// Serverless is the serverless function / service inventory.
	// Added in slice 1 of the serverless-tier arc (v0.89.90, #721
	// Stream 119). Covers AWS Lambda + GCP Cloud Run + GCP Cloud
	// Functions + Azure Functions + OCI Functions — five surfaces
	// across the four clouds. The proposer's recommendation surface
	// for serverless reasons about a TWO-axis detection rule:
	//
	//  1. HasTraceAxis — the cloud-native trace primitive is on
	//     (X-Ray active tracing for Lambda, Cloud Trace for Cloud
	//     Run / Functions, Application Insights for Azure Functions,
	//     APM for OCI Functions).
	//  2. HasOTelDistro — an OpenTelemetry distribution / layer /
	//     sidecar / env wrapper is attached.
	//
	// Either axis presence is informationally surfaced; neither axis
	// gates Result.InstrumentedCount on its own. The scanner-side
	// tally and the proposer-side reasoning denominate coverage on a
	// per-surface basis (see ServerlessInstanceSnapshot.IsInstrumented).
	//
	// See docs/proposals/serverless-tier-slice1.md §3 (detection
	// surfaces) and §11 (acceptance tests).
	Serverless []ServerlessInstanceSnapshot `json:"serverless"`

	// Orchestrations is the workflow / state-machine orchestrator
	// inventory. Added in slice 1 chunk 1 of the orchestration-tier
	// arc (v0.89.95, #728 Stream 126). Covers AWS Step Functions in
	// chunk 1; chunk 2 adds GCP Workflows and chunk 3 adds Azure
	// Logic Apps. OCI does not have a first-class orchestration
	// surface in slice 1 and contributes nothing here. The
	// proposer's recommendation surface for orchestration reasons
	// about a TWO-axis detection rule:
	//
	//  1. HasTraceAxis — the cloud-native trace primitive is on
	//     (X-Ray active tracing for Step Functions, the workflow's
	//     own trace export for GCP Workflows, App Insights trace
	//     routing for Logic Apps).
	//  2. HasLogAxis — a structured-logging destination is wired
	//     (Step Functions LoggingConfiguration.Level != OFF,
	//     Workflows callLogLevel != UNSPECIFIED, Logic Apps
	//     WorkflowRuntime diagnostic-setting routes to a sink).
	//
	// Either axis presence is informationally surfaced; neither axis
	// gates Result.InstrumentedCount on its own — slice 1 chunk 1
	// dispatches via the standalone ScanOrchestrations method on the
	// per-cloud scanners rather than the existing per-region walk
	// inside Scan(), so the tier-level counts are computed by the
	// handler after the dispatch returns.
	//
	// See docs/proposals/orchestration-tier-slice1.md §3 (detection
	// surfaces) and §11 (acceptance tests).
	Orchestrations []OrchestrationInstanceSnapshot `json:"orchestrations"`

	// EventSources is the inbound event source inventory — the cloud's
	// request entry point where trace IDs are created (or not). Added in
	// slice 1 chunk 1 of the event-source-tier arc (v0.89.100, #734
	// Stream 132). Covers AWS EventBridge in chunk 1; chunk 2 adds GCP
	// Pub/Sub, chunk 3 adds Azure Service Bus, chunk 4 adds OCI
	// Streaming. The proposer's recommendation surface for event sources
	// reasons about a TWO-axis detection rule (HasTraceAxis +
	// HasLogAxis) — same shape as the orchestration tier. Either-axis
	// presence is informationally surfaced; neither axis gates
	// Result.InstrumentedCount on its own.
	//
	// See docs/proposals/event-source-tier-slice1.md §3 (detection
	// surfaces) and §11 (acceptance tests).
	EventSources []EventSourceInstanceSnapshot `json:"event_sources"`

	// ECSClusters is the ECS (and future Cloud Run / AKS-style
	// container-orchestration) cluster inventory. Added in slice 5
	// of the universal-observation arc (v0.89.10). The proposer's
	// recommendation surface for ECS reasons about a SINGLE-axis
	// rule: cluster settings[name=containerInsights].value must be
	// "enabled". Same posture as the DynamoDB slice 4 single-axis
	// downgrade — cluster-level Container Insights is the one
	// strong cloud-API-visible observability signal for ECS, so
	// the rule is honest single-axis rather than inventing fake
	// axes from task-definition sidecars or FireLens routing.
	// Squadron emits enablement recommendations as plan steps;
	// never executes ecs:UpdateClusterSettings.
	//
	// Honest task-definition-level limitation: Squadron detects
	// cluster-level CloudWatch Container Insights. Squadron does
	// not detect task-definition-level instrumentation — X-Ray
	// daemon sidecars, ADOT collector sidecars, or FireLens log
	// routing in your task definitions. If your task defs include
	// those sidecars but the cluster does not have Container
	// Insights enabled, Squadron will report the cluster as
	// uninstrumented — this is a known limitation of cluster-level
	// scanning. A future slice can extend the rule to inspect task
	// definitions if operators request it.
	//
	// Both Fargate and EC2 launch types are covered by the same
	// per-cluster rule.
	ECSClusters []ECSClusterSnapshot `json:"ecs_clusters"`

	// InstrumentedCount sums Compute+Functions+Databases+ObjectStores+
	// LoadBalancers+Clusters+DynamoDBTables+ECSClusters entries where
	// observability presence was detected.
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
	//   - DynamoDBTables: ContributorInsightsStatus == "ENABLED".
	//     (Slice 4 single-axis rule. Squadron detects resource-side
	//     Contributor Insights; Squadron does not detect SDK-side
	//     OpenTelemetry or X-Ray instrumentation in your application
	//     code. If your DynamoDB SDK is OTel-wrapped on the client
	//     side, Squadron will report the table as uninstrumented —
	//     this is a known limitation of cloud-API-only scanning.)
	//   - ECSClusters: ContainerInsightsStatus == "enabled". (Slice
	//     5 single-axis rule on cluster-level CloudWatch Container
	//     Insights. Squadron does not detect task-definition-level
	//     instrumentation — X-Ray daemon sidecars, ADOT collector
	//     sidecars, or FireLens log routing in your task
	//     definitions. If your task defs include those sidecars but
	//     the cluster does not have Container Insights enabled,
	//     Squadron will report the cluster as uninstrumented — this
	//     is a known limitation of cluster-level scanning. A future
	//     slice can extend the rule to inspect task definitions if
	//     operators request it.)
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
	// As of v0.88.3 both PartialReason and FailedServices accumulate
	// across every service failure during a single scan. The AWS
	// scanner's recordPartialFailure helper joins PartialReason with
	// "; " separators when multiple service walks fail in the same
	// scan, and FailedServices is an append-only list. See
	// internal/discovery/aws/scanner.go::recordPartialFailure for the
	// accumulator implementation.
	FailedServices []string `json:"failed_services,omitempty"`
}

// ComputeInstanceSnapshot is the category-typed view of a virtual
// machine. Provider-specific scanners populate this from EC2
// DescribeInstances / GCE Instances.list / Azure VMs.list.
type ComputeInstanceSnapshot struct {
	// ResourceID is the provider-native ID: EC2 instance id / GCE
	// instance name / Azure VM id / VMware vmref.
	ResourceID string `json:"resource_id"`

	// ImportID — env->Terraform arc — the provider-canonical
	// `terraform import` ID when it differs from ResourceID. AWS:
	// empty (ResourceID already equals the import ID). Azure: the
	// full ARM resource ID. GCP: "project/zone/name". OCI: the OCID.
	// Empty means "no safe import ID captured" — the import-block
	// generator skips rather than guess.
	ImportID string `json:"import_id,omitempty"`

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

	// LastSeenAt — trace integration slice 1 chunk 4
	// (docs/proposals/trace-integration-slice1.md §6, v0.89.77).
	// Most recent timestamp at which Squadron's traceindex saw any
	// span from this resource. Nil means "no traces ever observed"
	// (rendered as "never" in the UI). Set at scan-response time by
	// joining against the traceindex on the projected resource key
	// (see traceindex.ComputeResourceKey and the inventory-side
	// ProjectComputeKey helper). Empty / unwired on the scanner-
	// produced result; the handler-side annotation step populates
	// it just before JSON emission.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
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

	// Slice 2 (database-tier-slice2.md, v0.89.63) — per-cloud
	// observability primitives. Each is provider-specific; the
	// proposer reads Provider plus the matching axis to decide
	// whether to emit a recommendation, and which kind. AWS RDS
	// slice 1 logic uses PerformanceInsightsEnabled +
	// EnhancedMonitoringEnabled (above) and these fields stay
	// zero — backward compat preserved.

	// QueryInsightsEnabled signals GCP Cloud SQL Query Insights
	// (settings.insightsConfig.queryInsightsEnabled). When false on
	// a Cloud SQL instance, the proposer emits a
	// cloudsql-pi-enable recommendation.
	QueryInsightsEnabled bool `json:"query_insights_enabled,omitempty"`

	// SQLInsightsDiagEnabled signals at least one Azure Diagnostic
	// Setting routing the SQLInsights log category to any
	// destination (Log Analytics, Storage, Event Hub) for the
	// instance. When false on an Azure SQL Database, the proposer
	// emits an azsql-diag-enable recommendation.
	SQLInsightsDiagEnabled bool `json:"sql_insights_diag_enabled,omitempty"`

	// DatabaseManagementEnabled signals OCI Operations Insights /
	// Database Management enrollment
	// (databaseManagementConfig.databaseManagementStatus ==
	// "ENABLED") on an OCI DB System or Autonomous Database. When
	// false, the proposer emits an ocidb-perfhub-enable
	// recommendation.
	DatabaseManagementEnabled bool `json:"database_management_enabled,omitempty"`

	// Provider discriminates which detection axis the proposer
	// reads. Empty defaults to "aws" for backward compatibility
	// with v0.87.0 audit rows. Slice 2 callers must set Provider
	// for non-AWS database snapshots so the proposer routes to
	// the right recommendation kind.
	Provider string `json:"provider,omitempty"`

	// LastSeenAt — trace integration slice 1 chunk 4
	// (docs/proposals/trace-integration-slice1.md §6, v0.89.77).
	// See ComputeInstanceSnapshot.LastSeenAt godoc for the join
	// semantics; the database-side projection key is documented on
	// traceindex.ProjectDatabaseKey.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
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

	// Slice 2 (kubernetes-tier-slice2.md, v0.89.68) — per-cloud
	// managed observability primitives. Each is provider-specific;
	// the proposer reads Provider plus the matching axis to decide
	// whether to emit a recommendation and which kind. AWS EKS slice
	// 1 logic uses ControlPlaneLogging + Addons (above) and these
	// fields stay zero — backward compat preserved.

	// ManagedPrometheusEnabled signals GCP GKE Google Cloud Managed
	// Service for Prometheus
	// (monitoringConfig.managedPrometheusConfig.enabled). When false
	// on a GKE cluster, the proposer emits a gke-mp-enable
	// recommendation.
	ManagedPrometheusEnabled bool `json:"managed_prometheus_enabled,omitempty"`

	// AzureMonitorEnabled signals Azure AKS managed observability
	// via any one of three addon profile flags
	// (addonProfiles.omsagent.enabled OR
	// azureMonitorProfile.metrics.enabled OR
	// azureMonitorProfile.containerInsights.enabled). When all three
	// are false on an AKS cluster, the proposer emits an
	// aks-monitor-enable recommendation. The three-way disjunction
	// mirrors EKS's "ADOT OR CloudWatch observability" pattern —
	// operators on either the legacy or newer addon get credit.
	AzureMonitorEnabled bool `json:"azure_monitor_enabled,omitempty"`

	// OperationsInsightsEnabled signals OCI OKE Operations Insights
	// enrollment via the operations-insights-enabled=true freeform
	// tag convention (slice 2 ships tag-based detection; slice 3
	// moves to a direct Operations Insights API call). When false
	// on an OKE cluster, the proposer emits an
	// oke-ops-insights-enable recommendation.
	OperationsInsightsEnabled bool `json:"operations_insights_enabled,omitempty"`

	// Provider discriminates which detection axis the proposer
	// reads. Empty defaults to "aws" for backward compatibility
	// with v0.89.0 audit rows. Slice 2 callers MUST set Provider
	// for non-AWS cluster snapshots so the proposer routes to the
	// right recommendation kind.
	Provider string `json:"provider,omitempty"`

	// LastSeenAt — trace integration slice 1 chunk 4
	// (docs/proposals/trace-integration-slice1.md §6, v0.89.77).
	// See ComputeInstanceSnapshot.LastSeenAt godoc for the join
	// semantics; the cluster-side projection key is documented on
	// traceindex.ProjectClusterKey.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
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

// DynamoDBTableSnapshot is the category-typed view of a managed
// NoSQL table. Provider-specific scanners populate this from
// DynamoDB ListTables + DescribeTable + DescribeContributorInsights
// / Cosmos DB account.tables.get / Cloud Bigtable instances.tables.
// The proposer reasons about category-level levers (Contributor
// Insights enablement on AWS, the equivalent throttling-and-hot-key
// surface on the other clouds) rather than provider-specific
// feature names.
//
// Slice 4's "instrumented" rule for NoSQL tables is single-axis:
// ContributorInsightsStatus must be "ENABLED". This is a deliberate
// downgrade from EKS slice 3b's composite rule — DynamoDB has
// exactly one cloud-API-visible observability signal per table
// that the operator must explicitly enable.
//
// SDK-side limitation (honestly stated): Squadron detects
// resource-side Contributor Insights; Squadron does not detect
// SDK-side OpenTelemetry or X-Ray instrumentation in your
// application code. If your DynamoDB SDK is OTel-wrapped on the
// client side, Squadron will report the table as uninstrumented —
// this is a known limitation of cloud-API-only scanning.
//
// The ContributorInsightsStatus field carries an additional
// sentinel value beyond what the AWS API enum exposes:
// "UNKNOWN". The scanner emits "UNKNOWN" when
// DescribeContributorInsights returned AccessDenied (the policy
// granted dynamodb:DescribeTable + dynamodb:ListTagsOfResource
// but not dynamodb:DescribeContributorInsights). The row is
// surfaced so the operator sees the inventory and can fix the
// policy; the "instrumented" rule counts UNKNOWN as
// uninstrumented because Squadron cannot prove coverage.
//
// Squadron does NOT execute dynamodb:UpdateContributorInsights —
// discovery is strictly read-only; the operator runs the
// enablement Terraform through their own IaC pipeline. Same
// posture as RDS's PI / EM and EKS's logging / addon levers.
type DynamoDBTableSnapshot struct {
	// ResourceID is the provider-native ID: DynamoDB table ARN /
	// Cosmos DB resource ID / Cloud Bigtable table name. The
	// scanner populates the full ARN for AWS so the proposer's
	// evidence list and the recommendation envelope's
	// AffectedResources field both surface the canonical
	// identifier.
	ResourceID string `json:"resource_id"`

	// Name is the operator-readable table name. For AWS this is
	// the trailing component of the ARN; kept separate so the UI
	// doesn't have to parse ARNs.
	Name string `json:"name"`

	// Status is the provider-typed table status string:
	// "ACTIVE" / "CREATING" / "UPDATING" / "DELETING" /
	// "INACCESSIBLE_ENCRYPTION_CREDENTIALS" / "ARCHIVING" /
	// "ARCHIVED". Surfaced raw so the Inventory tab can dim
	// non-ACTIVE rows and the proposer can decline to recommend
	// against a non-ACTIVE table.
	Status string `json:"status"`

	// BillingMode is the table's capacity-mode string:
	// "PROVISIONED" or "PAY_PER_REQUEST". Surfaced
	// informationally — the proposer doesn't reason about it
	// today, but the Inventory tab renders it alongside the
	// observability signal so the operator can see whether
	// enabling Contributor Insights against a high-throughput
	// PAY_PER_REQUEST table is going to add cost. Empty when the
	// table has no BillingModeSummary (older tables created
	// before PAY_PER_REQUEST existed default to PROVISIONED but
	// don't populate the summary block).
	BillingMode string `json:"billing_mode,omitempty"`

	// ContributorInsightsStatus is the single observability-rule
	// axis. AWS enum values: "ENABLED" / "DISABLED" / "ENABLING"
	// / "DISABLING" / "FAILED". The scanner also emits the
	// sentinel "UNKNOWN" when DescribeContributorInsights itself
	// returned AccessDenied (see type godoc). The "instrumented"
	// rule treats only "ENABLED" as covered; every other value
	// (including UNKNOWN) counts as uninstrumented.
	ContributorInsightsStatus string `json:"contributor_insights_status"`

	// Region is where the table lives.
	Region string `json:"region"`

	// Tags follows the same flattened shape as the other category
	// snapshots.
	Tags map[string]string `json:"tags,omitempty"`
}

// IsInstrumented implements the slice 4 single-axis rule for
// DynamoDB tables: a table counts as instrumented iff
// ContributorInsightsStatus == "ENABLED". Every other value
// (DISABLED / ENABLING / DISABLING / FAILED / UNKNOWN) counts as
// uninstrumented. Kept as a method on the snapshot so the
// scanner-side tally, the proposer-side reasoning, and the
// Inventory tab can all reference the same predicate.
func (t DynamoDBTableSnapshot) IsInstrumented() bool {
	return t.ContributorInsightsStatus == "ENABLED"
}

// ECSClusterSnapshot is the category-typed view of an ECS cluster.
// Provider-specific scanners populate this from ECS ListClusters +
// DescribeClusters / Cloud Run services list / AKS clusters list.
// The proposer reasons about cluster-level container observability
// (CloudWatch Container Insights on AWS, the equivalent surfaces on
// the other clouds) rather than provider-specific feature names.
//
// Slice 5's "instrumented" rule for ECS clusters is single-axis:
// settings[name=containerInsights].value == "enabled". This matches
// the DynamoDB slice 4 honest single-axis posture — cluster-level
// Container Insights is the one strong cloud-API-visible
// observability signal for ECS, so the rule is honest single-axis
// rather than inventing fake axes from task-definition sidecars or
// FireLens routing.
//
// Honest task-definition-level limitation (re-stated honestly):
// Squadron detects cluster-level CloudWatch Container Insights.
// Squadron does not detect task-definition-level instrumentation —
// X-Ray daemon sidecars, ADOT collector sidecars, or FireLens log
// routing in your task definitions. If your task defs include
// those sidecars but the cluster does not have Container Insights
// enabled, Squadron will report the cluster as uninstrumented —
// this is a known limitation of cluster-level scanning. A future
// slice can extend the rule to inspect task definitions if
// operators request it.
//
// Both Fargate and EC2 launch types are covered by the same
// per-cluster rule — Container Insights is per-cluster, not
// per-launch-type.
//
// The ContainerInsightsStatus field carries the four AWS API
// enum-style values plus the scanner's "UNKNOWN" sentinel: AWS
// returns "enabled" / "disabled" / "enhanced" (the
// enhanced-observability tier added in ECS 2024) at the cluster
// settings layer; the scanner emits "UNKNOWN" when the
// DescribeClusters response did not surface the
// containerInsights setting for the cluster (typical when the
// operator's policy granted ecs:DescribeClusters but not the
// SETTINGS include hint at the call layer). The "instrumented"
// rule treats only "enabled" (case-insensitive) as covered;
// every other value (including UNKNOWN) counts as uninstrumented.
//
// Squadron does NOT execute ecs:UpdateClusterSettings — discovery
// is strictly read-only; the operator runs the enablement
// Terraform through their own IaC pipeline. Same posture as
// RDS's PI / EM and EKS's logging / addon levers, and DynamoDB's
// Contributor Insights lever.
type ECSClusterSnapshot struct {
	// Name is the operator-readable cluster name (the trailing
	// component of the ARN). Kept separate so the UI doesn't have
	// to parse ARNs.
	Name string `json:"name"`

	// ARN is the full Amazon Resource Name. The scanner populates
	// the canonical identifier so the proposer's evidence list and
	// the recommendation envelope's AffectedResources field both
	// reference it.
	ARN string `json:"arn"`

	// Status is the provider-typed cluster status string:
	// "ACTIVE" / "PROVISIONING" / "DEPROVISIONING" / "FAILED" /
	// "INACTIVE". Surfaced raw so the Inventory tab can dim
	// non-ACTIVE rows and the proposer can decline to recommend
	// against a non-ACTIVE cluster.
	Status string `json:"status"`

	// ContainerInsightsStatus is the single observability-rule
	// axis. AWS-style values: "enabled" / "disabled" / "enhanced".
	// The scanner also emits the sentinel "UNKNOWN" when the
	// DescribeClusters response did not surface the
	// containerInsights setting for the cluster (see type godoc).
	// The "instrumented" rule treats only "enabled"
	// (case-insensitive) as covered; every other value (including
	// UNKNOWN) counts as uninstrumented.
	ContainerInsightsStatus string `json:"container_insights_status"`

	// RegisteredContainerInstancesCount is the operator-visible
	// count of EC2 container instances registered to the cluster.
	// Surfaced informationally so the Inventory tab can hint at the
	// launch-type mix — high counts here signal an EC2-heavy
	// posture; zero suggests Fargate-only. The proposer's rule
	// does not gate on it; the count is purely informational.
	RegisteredContainerInstancesCount int `json:"registered_container_instances_count"`

	// RunningTasksCount is the cluster's running-task tally
	// (Fargate + EC2 launch types combined). Informational.
	RunningTasksCount int `json:"running_tasks_count"`

	// PendingTasksCount is the cluster's pending-task tally.
	// Informational — a high pending count flagged alongside a
	// disabled Container Insights status is a cluster the
	// proposer is likely to surface first.
	PendingTasksCount int `json:"pending_tasks_count"`

	// ActiveServicesCount is the cluster's active-service tally.
	// Informational.
	ActiveServicesCount int `json:"active_services_count"`

	// Region is where the cluster lives.
	Region string `json:"region"`

	// Tags follows the same flattened shape as the other category
	// snapshots.
	Tags map[string]string `json:"tags,omitempty"`
}

// IsInstrumented implements the slice 5 single-axis rule for ECS
// clusters: a cluster counts as instrumented iff
// ContainerInsightsStatus == "enabled" (case-insensitive — the AWS
// SDK returns lowercase, defense-in-depth costs nothing). Every
// other value ("disabled" / "enhanced" / UNKNOWN / empty) counts as
// uninstrumented per the slice 5 honest rule. Note: "enhanced" is
// the new ECS enhanced-observability tier; the slice 5 rule treats
// it as uninstrumented because Squadron cannot prove parity with
// the standard Container Insights signal surface from the
// cloud-API response alone. A future slice can broaden the rule
// to count "enhanced" as covered if operators request it.
//
// Kept as a method on the snapshot so the scanner-side tally, the
// proposer-side reasoning, and the Inventory tab can all reference
// the same predicate.
func (c ECSClusterSnapshot) IsInstrumented() bool {
	return strings.EqualFold(c.ContainerInsightsStatus, "enabled")
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

// ServerlessInstanceSnapshot is the category-typed view of a
// serverless function or service. Provider-specific scanners populate
// this from Lambda ListFunctions (AWS) / Cloud Run services.list (GCP)
// / cloudfunctions.list (GCP) / Microsoft.Web/sites?$filter=kind eq
// 'functionapp' (Azure) / functions.ListFunctions (OCI). The proposer
// reasons about category-level levers (trace primitive enabled, OTel
// distro attached) rather than provider-specific feature names.
//
// Slice 1 of the serverless-tier arc (v0.89.90, #721 Stream 119)
// detects per-surface:
//
//  1. HasTraceAxis — the cloud-native trace primitive is on. AWS:
//     X-Ray active tracing. GCP Cloud Run: Cloud Trace integration.
//     GCP Cloud Functions: GOOGLE_CLOUD_TRACE env var. Azure: App
//     Insights connection string set. OCI: OCI_APM_ENABLED config.
//  2. HasOTelDistro — an OpenTelemetry distribution is attached.
//     AWS: ADOT layer ARN matches the canonical prefix, or the
//     AWS_LAMBDA_EXEC_WRAPPER env var is set. GCP Cloud Run: a
//     sidecar container named "otel-collector*" or
//     OTEL_EXPORTER_OTLP_ENDPOINT is set. GCP Cloud Functions: OTel
//     layer attached. Azure: OTEL_DOTNET_AUTO_HOME or
//     OTEL_PYTHON_DISTRO app setting. OCI: OTEL_DISTRO config.
//
// The two axes are surfaced as independent booleans; the proposer's
// per-surface recommendation kinds (lambda-xray-active,
// lambda-otel-layer, lambda-otel-wrapper, ...) key off whichever axis
// is missing. Slice 1 does NOT collapse the two into a single
// "instrumented" predicate at the Result level — the per-surface
// rules differ enough that a composite-rule predicate would either
// over- or under-count.
//
// Surface-specific detail (Lambda's x_ray_mode + layer_count, Cloud
// Run's container names, Azure Functions' app settings, OCI's
// config map) lives in the Detail bag so the per-cloud Inventory
// tabs can render provider-specific context without forcing a
// universal struct shape.
//
// Squadron does NOT execute any modify call — discovery is strictly
// read-only; the operator runs the enablement Terraform through their
// own IaC pipeline. Same posture as the compute / database / cluster
// / DynamoDB / ECS levers.
//
// See docs/proposals/serverless-tier-slice1.md §3 (per-cloud detection
// axes), §5 (scanner contract), §11 acceptance tests 1-9, §12 (threat
// model — the Lambda layer ARN whitelist drift mitigation).
type ServerlessInstanceSnapshot struct {
	// Provider is the cloud name — "aws" / "gcp" / "azure" / "oci".
	// Discriminator for the Detail bag's shape.
	Provider string `json:"provider"`

	// Surface is the per-cloud serverless surface identifier —
	// "lambda" / "cloudrun" / "cloudfunc" / "azfunc" / "ocifunc".
	// Drives the proposer's recommendation-kind prefix routing
	// (lambda-* → AWS, cloudrun-* / cloudfunc-* → GCP, azfunc-* →
	// Azure, ocifunc-* → OCI).
	Surface string `json:"surface"`

	// AccountID is the provider-native primary identifier of the
	// owning connection (account_id / project_id / subscription_id /
	// tenancy OCID).
	AccountID string `json:"account_id"`

	// Region is where the function / service lives. Serverless
	// surfaces are per-region on every cloud Squadron supports.
	Region string `json:"region"`

	// ResourceName is the operator-readable name. For Lambda this is
	// the function name; for Cloud Run a service name; for Cloud
	// Functions a function name; for Azure Functions a site name;
	// for OCI Functions a function name.
	ResourceName string `json:"resource_name"`

	// ResourceARN is the provider-native fully-qualified resource
	// identifier. Lambda ARN / Cloud Run service self-link / Cloud
	// Functions resource path / Azure Functions resource ID / OCI
	// Functions OCID. Carries the canonical handle the proposer's
	// evidence list and the recommendation envelope's
	// AffectedResources field both reference.
	ResourceARN string `json:"resource_arn"`

	// Runtime is the provider-typed runtime string: "nodejs20.x" /
	// "python3.11" / "dotnet6" / "go1.21" / etc. Drives the
	// proposer's per-language SDK customization in slice 2; surfaced
	// raw in slice 1.
	Runtime string `json:"runtime,omitempty"`

	// HasTraceAxis signals the cloud-native trace primitive is on
	// (see type godoc for per-cloud detection). One of the two axes
	// the proposer's recommendation kinds key off.
	HasTraceAxis bool `json:"has_trace_axis"`

	// HasOTelDistro signals an OpenTelemetry distribution / layer /
	// sidecar / env wrapper is attached. The other axis.
	HasOTelDistro bool `json:"has_otel_distro"`

	// LastSeenAt — slice 1 trace integration parity (see
	// ComputeInstanceSnapshot.LastSeenAt godoc for the join
	// semantics). Most recent timestamp at which Squadron's
	// traceindex saw any span from this resource. Nil means "no
	// traces ever observed" (rendered as "never" in the UI). Set at
	// scan-response time by joining against the traceindex on the
	// per-surface projection key; empty on the scanner-produced
	// result.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`

	// ColdStartP95Ms — Cold-start latency analysis slice 1 chunk 3
	// (v0.89.115, #753 Stream 151). Most recent 24-hour P95
	// InitDuration observation for this Lambda function, sourced
	// from the cold_start_observation table at scan-response time.
	// Nil means "no observation persisted yet" (rendered as "—" in
	// the UI). Set by the cold-start annotation pass that runs
	// AFTER the trace-emission LastSeenAt annotation; empty on the
	// scanner-produced result. Slice 1 ships AWS Lambda only —
	// GCP / Azure / OCI rows leave the field nil through slice 2.
	ColdStartP95Ms *float64 `json:"cold_start_p95_ms,omitempty"`

	// ColdStartExceedsThreshold — Cold-start latency analysis slice 1
	// chunk 3 (v0.89.115). Pre-computed amber-color predicate the UI
	// reads to color the Cold-start P95 cell: true when the chunk-2
	// detection's ShouldFireRecommendation predicate held for the
	// most recent observation pair (24h vs. 7d baseline). Nil when
	// the field above is also nil (no observation). Slice 1 ships
	// AWS Lambda only.
	ColdStartExceedsThreshold *bool `json:"cold_start_exceeds_threshold,omitempty"`

	// SamplingRatio — Sampling rate analysis slice 1 chunk 2
	// (v0.89.123, #763 Stream 161). Most recent 24-hour sampling
	// ratio (observed_span_count / expected_invocation_count) for
	// this serverless resource, populated by the sampling
	// annotation pass at scan-response time. Nil means "no
	// observation persisted yet" (rendered as "—" in the UI). The
	// chunk-3 UI will render this as a "Sampling rate (24h)"
	// column between Cold-start P95 and Last seen.
	SamplingRatio *float64 `json:"sampling_ratio,omitempty"`

	// SamplingExceedsFloor — Sampling rate analysis slice 1 chunk 2
	// (v0.89.123). Pre-computed amber-color predicate the UI reads
	// to color the Sampling rate cell: true when the chunk-2
	// detection's ShouldFireRecommendation predicate held (ratio
	// below 5% AND invocation count above 1000). Nil when the
	// field above is also nil (no observation, or invocation count
	// below the statistical noise minimum). Surfaces in the
	// chunk-3 tooltip alongside the underlying invocation count.
	SamplingExceedsFloor *bool `json:"sampling_exceeds_floor,omitempty"`

	// CurrentErrorRate — Error rate correlation slice 1 chunk 3
	// (v0.89.129, #769 Stream 167). Most recent 24-hour current
	// error rate (current_error_count / current_invocation_count)
	// for this serverless resource, populated by the error-rate
	// annotation pass at scan-response time. Nil means "no
	// observation persisted yet" (rendered as "—" in the UI). The
	// chunk-3 UI renders this as an "Error rate (24h)" column
	// between Sampling rate and Last seen on all 4 provider
	// Serverless tables.
	CurrentErrorRate *float64 `json:"current_error_rate,omitempty"`

	// ErrorRateExceedsThreshold — Error rate correlation slice 1
	// chunk 3 (v0.89.129). Pre-computed amber-color predicate the
	// UI reads to color the Error rate cell: true when the
	// chunk-2 detection's ShouldFireRecommendation predicate held
	// (current/baseline ratio > 2.0x AND current invocations >=
	// 1000 AND current errors >= 50). Nil when the field above is
	// also nil (no observation).
	ErrorRateExceedsThreshold *bool `json:"error_rate_exceeds_threshold,omitempty"`

	// Detail is the per-surface bag the per-cloud Inventory tabs
	// render alongside the universal columns. Lambda populates
	// {"x_ray_mode": "Active"/"PassThrough"/"", "layer_count": N};
	// Cloud Run populates {"container_count": N,
	// "sidecar_names": ["otel-collector"]}; Azure populates a slim
	// app_settings subset; OCI populates a config subset. Empty when
	// no surface-specific context applies.
	Detail map[string]any `json:"detail,omitempty"`
}

// ScanScope is the per-call narrowing envelope the serverless-tier
// ScanServerless methods accept (docs/proposals/serverless-tier-
// slice1.md §5 scanner contract). The chunk-5 trampoline passes a
// ScanScope per invocation so the scanner can target a specific
// account / project / subscription / tenancy + (optionally) a
// narrower compartment / resource-group / project subset than the
// scanner's connection-wide default. An empty ScanScope means "scan
// the full default surface the scanner is configured for".
//
// For the chunk-4 OCI Functions scanner the relevant fields are
// AccountID (overrides the per-snapshot AccountID stamped onto
// every row — the scanner falls back to its configured TenancyOCID
// when empty) and CompartmentIDs (overrides the default "tenancy
// root + first-level children" walk; the scanner walks exactly the
// listed compartments when set, skipping the Identity API call
// entirely). The other fields are reserved for the GCP / Azure /
// AWS chunk-2 / 3 scanners' equivalent narrowing.
//
// Kept as a struct (not a function-args bag) so future per-tier
// scope expansions land on the same envelope without churning the
// per-scanner method signatures.
type ScanScope struct {
	// AccountID overrides the per-snapshot AccountID stamped on
	// every row. Empty means "use the scanner's configured
	// connection-level account id" (TenancyOCID on OCI,
	// ProjectID on GCP, SubscriptionID on Azure, the assumed-role
	// account on AWS).
	AccountID string `json:"account_id,omitempty"`

	// CompartmentIDs narrows the walk to a specific subset of OCI
	// compartments. Empty defaults to "tenancy root + first-level
	// children" via the existing listCompartments helper. Reserved
	// here as the canonical OCI-side narrowing slot; the GCP /
	// Azure / AWS scanners read their own per-cloud narrowing
	// fields below.
	CompartmentIDs []string `json:"compartment_ids,omitempty"`

	// ResourceGroups narrows an Azure walk to specific
	// resource-group names. Reserved for the chunk-3 Azure
	// Functions scanner.
	ResourceGroups []string `json:"resource_groups,omitempty"`

	// Projects narrows a GCP walk to specific project IDs.
	// Reserved for the chunk-2 GCP Cloud Run / Cloud Functions
	// scanners.
	Projects []string `json:"projects,omitempty"`

	// Regions narrows the walk to a subset of regions. Empty
	// means "scan every region the scanner's connection is
	// configured for". Reserved across all four clouds; slice 1
	// ships single-region per connection so the chunk-4 OCI
	// scanner ignores this field.
	Regions []string `json:"regions,omitempty"`
}

// IsInstrumented implements the slice 1 OR-rule for serverless
// resources: a function or service counts as instrumented when at
// least one of the two axes (HasTraceAxis OR HasOTelDistro) is true.
//
// Rationale: serverless surfaces differ from the compute / database /
// cluster tiers in that the cloud-native trace primitive (X-Ray, Cloud
// Trace, App Insights, APM) is itself a meaningful observability
// signal — operators frequently ship to the cloud's native APM and
// do NOT layer OTel on top. The composite "both axes" rule we use
// for EKS would over-count uninstrumented serverless deployments;
// the single-axis "HasOTelDistro only" rule we use for compute would
// under-count operators on the native APM. Slice 1 lands the OR
// rule; slice 2's per-surface span-quality probe can tighten the
// predicate per surface if operator feedback warrants it.
//
// Kept as a method on the snapshot so the scanner-side tally, the
// proposer-side reasoning, and the per-cloud Inventory tab can all
// reference the same predicate.
func (s ServerlessInstanceSnapshot) IsInstrumented() bool {
	return s.HasTraceAxis || s.HasOTelDistro
}

// OrchestrationInstanceSnapshot is the category-typed view of a
// workflow / state-machine orchestrator inventory row. v0.89.95 (#728
// Stream 126, slice 1 chunk 1 of the Orchestration tier arc). Per-cloud
// scanners populate this from AWS Step Functions ListStateMachines /
// GCP Workflows projects.locations.workflows.list / Azure Resource
// Graph (Microsoft.Logic/workflows). The proposer reasons about
// category-level levers (trace primitive enabled, structured-logging
// destination wired) rather than provider-specific feature names.
//
// Slice 1 chunk 1 ships the AWS Step Functions scanner only; the GCP
// Workflows scanner lands in chunk 2 and the Azure Logic Apps scanner
// lands in chunk 3. OCI does not have a first-class orchestration
// surface in slice 1 and returns nil from any future ScanOrchestrations
// call until slice 2.
//
// The two axes the proposer reasons about per surface
// (docs/proposals/orchestration-tier-slice1.md §3):
//
//  1. HasTraceAxis — the cloud-native distributed trace primitive is on
//     for the workflow. AWS Step Functions: TracingConfiguration.Enabled
//     == true (X-Ray active tracing). GCP Workflows: callLogLevel
//     includes the trace export (slice 2 chunk 2). Azure Logic Apps:
//     diagnostic-settings exports the WorkflowRuntime category to App
//     Insights (slice 2 chunk 3).
//  2. HasLogAxis — a structured-logging destination is wired. AWS Step
//     Functions: LoggingConfiguration.Level != OFF and != "". GCP
//     Workflows: callLogLevel != CALL_LOG_LEVEL_UNSPECIFIED. Azure Logic
//     Apps: at least one diagnostic-setting routes the WorkflowRuntime
//     category to a log sink.
//
// The two axes are surfaced as independent booleans; the proposer's
// per-surface recommendation kinds (stepfunc-xray-active,
// stepfunc-logging-enabled, ...) key off whichever axis is missing.
// Slice 1 does NOT collapse the two into a single "instrumented"
// predicate at the Result level — the per-surface rules differ enough
// that a composite predicate would either over- or under-count.
//
// Surface-specific detail (Step Functions' workflow_type discriminating
// STANDARD vs EXPRESS, Logic Apps' sku / consumption posture, Workflows'
// region-projection) lives in the Detail bag so the per-cloud Inventory
// tabs can render provider-specific context without forcing a universal
// struct shape.
//
// Squadron does NOT execute any modify call — discovery is strictly
// read-only; the operator runs the enablement Terraform through their
// own IaC pipeline. Same posture as the compute / database / cluster /
// serverless tiers.
//
// EXPRESS coverage caveat (design doc §12): Step Functions EXPRESS
// workflows surface the same TracingConfiguration + LoggingConfiguration
// fields as STANDARD workflows, but in practice EXPRESS shops route logs
// through CloudWatch metrics rather than per-execution log groups. The
// slice 1 detection rule treats EXPRESS identically to STANDARD; if
// operator feedback shows EXPRESS workflows are under-counted as
// "unlogged", slice 2 can introduce a surface-type-aware variant.
//
// See docs/proposals/orchestration-tier-slice1.md §3 (per-cloud
// detection axes), §4 (storage schema), §5 (scanner contract), §12
// (EXPRESS coverage caveat + threat model).
type OrchestrationInstanceSnapshot struct {
	// Provider is the cloud name — "aws" / "gcp" / "azure".
	// Discriminator for the Detail bag's shape. OCI is unsupported
	// in slice 1.
	Provider string `json:"provider"`

	// Surface is the per-cloud orchestration surface identifier —
	// "stepfunc" / "workflows" / "logicapps". Drives the proposer's
	// recommendation-kind prefix routing (stepfunc-* → AWS,
	// workflows-* → GCP, logicapps-* → Azure).
	Surface string `json:"surface"`

	// AccountID is the provider-native primary identifier of the
	// owning connection (account_id / project_id / subscription_id).
	AccountID string `json:"account_id"`

	// Region is where the state machine / workflow lives. All three
	// supported clouds expose orchestrators as per-region resources.
	Region string `json:"region"`

	// ResourceName is the operator-readable name. For Step Functions
	// this is the state-machine name; for Workflows the workflow id;
	// for Logic Apps the workflow's resource name.
	ResourceName string `json:"resource_name"`

	// ResourceARN is the provider-native fully-qualified resource
	// identifier. Step Functions state-machine ARN / Workflows
	// resource path / Logic Apps resource ID. Carries the canonical
	// handle the proposer's evidence list and the recommendation
	// envelope's AffectedResources field both reference.
	ResourceARN string `json:"resource_arn"`

	// WorkflowType is the per-surface subtype string. Step Functions:
	// "STANDARD" or "EXPRESS". Logic Apps: "Standard" (App Service
	// hosted) or "Consumption" (multi-tenant). Workflows: the API
	// has a single workflow type; this field stays empty there.
	WorkflowType string `json:"workflow_type,omitempty"`

	// HasTraceAxis signals the cloud-native trace primitive is on
	// for this workflow (see type godoc for per-cloud detection).
	// One of the two axes the proposer's recommendation kinds key
	// off.
	HasTraceAxis bool `json:"has_trace_axis"`

	// HasLogAxis signals a structured-logging destination is wired
	// for this workflow. The other axis.
	HasLogAxis bool `json:"has_log_axis"`

	// LastSeenAt — slice 1 trace integration parity (see
	// ComputeInstanceSnapshot.LastSeenAt godoc for the join
	// semantics). Most recent timestamp at which Squadron's
	// traceindex saw any span tagged for this workflow. Nil means
	// "no traces ever observed" (rendered as "never" in the UI).
	// Set at scan-response time by joining against the traceindex
	// on the per-surface projection key; empty on the scanner-
	// produced result. Slice 1 chunk 1 leaves the join unwired
	// (chunk 4 of the arc adds it).
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`

	// Detail is the per-surface bag the per-cloud Inventory tabs
	// render alongside the universal columns. Step Functions
	// populates {"workflow_type": "STANDARD"/"EXPRESS"}; Workflows
	// populates {"call_log_level": "..."}; Logic Apps populates a
	// slim diagnostic-settings subset. Empty when no surface-
	// specific context applies.
	Detail map[string]any `json:"detail,omitempty"`
}

// IsInstrumented implements the slice 1 OR-rule for orchestration
// resources: a workflow counts as instrumented when at least one of the
// two axes (HasTraceAxis OR HasLogAxis) is true.
//
// Rationale: like the serverless tier (and unlike compute / database /
// cluster), the cloud-native trace primitive (X-Ray for Step Functions,
// the workflow's own log export, App Insights for Logic Apps) is itself
// a meaningful observability signal — operators frequently ship to the
// cloud's native observability surface and do NOT layer OTel on top. A
// composite "both axes" rule would over-count uninstrumented workflows;
// a single-axis rule would under-count operators on the native trace
// primitive. Slice 1 lands the OR rule and reserves slice 2 to tighten
// it per surface if operator feedback warrants.
//
// Kept as a method on the snapshot so the scanner-side tally, the
// proposer-side reasoning, and the per-cloud Inventory tab all reference
// the same predicate.
func (s OrchestrationInstanceSnapshot) IsInstrumented() bool {
	return s.HasTraceAxis || s.HasLogAxis
}

// EventSourceInstanceSnapshot is the category-typed view of an inbound
// event source — the surface where a trace ID is created (or fails to be
// created) at the cloud's request entry point. v0.89.100 (#734 Stream 132,
// slice 1 chunk 1 of the Event source tier arc). Per-cloud scanners populate
// this from AWS EventBridge ListEventBuses + ListRules + ListTargetsByRule
// (chunk 1), GCP Pub/Sub projects.topics.list (chunk 2), Azure Service Bus
// namespaces.list (chunk 3), OCI Streaming ListStreams (chunk 4).
//
// Slice 1 chunk 1 ships the AWS EventBridge scanner only; chunks 2-4 add
// the other three clouds. The proposer reasons about category-level levers
// (trace primitive enabled at the source, structured-logging destination
// wired) rather than per-message propagation — the per-message analysis is
// substantially harder and slated for slice 2.
//
// The two axes the proposer reasons about per surface
// (docs/proposals/event-source-tier-slice1.md §3):
//
//  1. HasTraceAxis — the cloud-native trace primitive is on for the event
//     source. AWS EventBridge: Schemas Discoverer in "Active" state OR (slice
//     1 chunk 1 fallback) at least one rule with a CloudWatch Logs target.
//     GCP Pub/Sub: tracingConfig.samplingRatio > 0 (chunk 2). Azure Service
//     Bus: a diagnostic-setting routes WorkflowRuntime to App Insights or
//     Log Analytics (chunk 3). OCI Streaming: OCI Logging captures the
//     stream's events (chunk 4).
//  2. HasLogAxis — a structured-logging destination is wired. AWS
//     EventBridge: rule has a target whose ARN is a CloudWatch Logs log
//     group. GCP Pub/Sub: schemaSettings.schema set OR log sink configured
//     (chunk 2). Azure Service Bus: diagnostic-settings routes to Log
//     Analytics (chunk 3). OCI Streaming: Logging log group attached
//     (chunk 4).
//
// EventBridge Schemas Discoverer detection softness (design doc §12):
// Slice 1 chunk 1 treats the log-target axis as the proxy for BOTH
// HasTraceAxis AND HasLogAxis because the Schemas Discoverer API lives in
// a separate SDK package (github.com/aws/aws-sdk-go-v2/service/schemas)
// and the chunk-1 budget (~1300 lines) does not accommodate the
// additional client wiring + IAM action set. The log-target proxy is
// honest: a bus with a CloudWatch Logs target is a strong proxy for trace
// observability readiness — CloudWatch Logs can carry the X-Ray trace
// header. Slice 2 lands the direct Schemas Discoverer detection and
// separates the two axes per the design doc's three-axis table.
//
// Squadron does NOT execute any modify call — discovery is strictly
// read-only; the operator runs the enablement Terraform through their own
// IaC pipeline. Same posture as the compute / database / cluster /
// serverless / orchestration tiers.
//
// See docs/proposals/event-source-tier-slice1.md §3 (per-cloud detection
// axes), §4 (storage schema), §5 (scanner contract), §11 acceptance
// tests 1-3, §12 (threat model on Schemas detection softness).
type EventSourceInstanceSnapshot struct {
	// Provider is the cloud name — "aws" / "gcp" / "azure" / "oci".
	// Discriminator for the Detail bag's shape.
	Provider string `json:"provider"`

	// Surface is the per-cloud event source surface identifier —
	// "eventbridge" / "pubsub" / "servicebus" / "streaming". Drives the
	// proposer's recommendation-kind prefix routing (eventbridge-* →
	// AWS, pubsub-* → GCP, servicebus-* → Azure, streaming-* → OCI).
	Surface string `json:"surface"`

	// AccountID is the provider-native primary identifier of the owning
	// connection (account_id / project_id / subscription_id /
	// tenancy OCID).
	AccountID string `json:"account_id"`

	// Region is where the event source lives. EventBridge is per-region;
	// Pub/Sub topics are global with locational hints; Service Bus is
	// per-resource-group; Streaming is per-region.
	Region string `json:"region"`

	// ResourceName is the operator-readable name. For EventBridge this
	// is the event-bus name; for Pub/Sub the topic id; for Service Bus
	// the namespace name; for Streaming the stream name.
	ResourceName string `json:"resource_name"`

	// ResourceARN is the provider-native fully-qualified resource
	// identifier. EventBridge ARN / Pub/Sub resource path / Service Bus
	// resource ID / Streaming OCID. Carries the canonical handle the
	// proposer's evidence list and the recommendation envelope's
	// AffectedResources field both reference.
	ResourceARN string `json:"resource_arn"`

	// SourceType is the per-surface subtype string. EventBridge: "bus".
	// Pub/Sub: "topic". Service Bus: "namespace" / "queue" / "topic".
	// Streaming: "stream". Surfaced so the per-cloud Inventory tab can
	// render a typed column without parsing ResourceARN.
	SourceType string `json:"source_type,omitempty"`

	// HasTraceAxis signals the cloud-native trace primitive is on for
	// this event source (see type godoc for per-cloud detection). One
	// of the two axes the proposer's recommendation kinds key off.
	HasTraceAxis bool `json:"has_trace_axis"`

	// HasLogAxis signals a structured-logging destination is wired for
	// this event source. The other axis.
	HasLogAxis bool `json:"has_log_axis"`

	// LastSeenAt — slice 1 trace integration parity (see
	// ComputeInstanceSnapshot.LastSeenAt godoc for the join semantics).
	// Most recent timestamp at which Squadron's traceindex saw any span
	// tagged for this event source. Nil means "no traces ever observed"
	// (rendered as "never" in the UI). Set at scan-response time by
	// joining against the traceindex on the per-surface projection key;
	// empty on the scanner-produced result. Slice 1 chunk 1 leaves the
	// join unwired (chunk 5 of the arc adds it).
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`

	// Detail is the per-surface bag the per-cloud Inventory tabs render
	// alongside the universal columns. EventBridge populates
	// {"rule_count": N}; Pub/Sub populates a schema-settings subset;
	// Service Bus populates a diagnostic-settings subset; Streaming
	// populates a logging-target subset. Empty when no surface-specific
	// context applies.
	Detail map[string]any `json:"detail,omitempty"`

	// Slice 2 (v0.89.105, #741 Stream 139): per-message propagation
	// detection. The slice-1 axes above answer "is the cloud-native
	// trace primitive enabled on the event source?" The slice-2 axis
	// goes deeper: does the event source's control-plane CONFIG
	// preserve trace context end-to-end so a downstream consumer span
	// can correlate to the upstream publisher span?
	//
	// HasPropagationConfig is true when every per-target / per-schema /
	// per-policy detection on the surface returns "preserved". On
	// EventBridge: every rule's targets have either no InputPath /
	// InputTransformer, an InputPath of "$", or an InputTransformer
	// template that includes the X-Ray (x-amzn-trace-id) or W3C
	// (traceparent) header string. False if ANY broken target exists
	// on ANY rule on the bus — propagation is a worst-case axis.
	//
	// A surface with NO rules / NO schema / NO subscriptions to inspect
	// defaults to TRUE (vacuously preserved — there's nothing to break
	// propagation).
	//
	// PropagationNotes carries human-readable per-issue strings, e.g.
	// "rule 'order-events' has InputPath '$.detail' that strips trace
	// header". The proposer uses these notes in recommendation
	// reasoning text in chunk 5; the UI side panel renders them when
	// the operator clicks the propagation column on the Event sources
	// sub-tab. Empty when HasPropagationConfig is true.
	//
	// See docs/proposals/event-source-tier-slice2.md §3 for the
	// per-cloud detection logic. Slice 2 chunk 1 (v0.89.105) ships AWS
	// EventBridge per-rule detection; chunks 2-4 add GCP Pub/Sub,
	// Azure Service Bus, OCI Streaming.
	HasPropagationConfig bool     `json:"has_propagation_config"`
	PropagationNotes     []string `json:"propagation_notes,omitempty"`
}

// IsInstrumented implements the slice 1 OR-rule for event source
// resources: an event source counts as instrumented when at least one of
// the two axes (HasTraceAxis OR HasLogAxis) is true.
//
// Rationale: same as the serverless and orchestration tiers — the
// cloud-native trace primitive (Schemas Discoverer for EventBridge,
// tracingConfig for Pub/Sub, diagnostic-settings for Service Bus, OCI
// Logging for Streaming) is itself a meaningful observability signal.
// Operators frequently ship to the cloud's native observability surface
// and do NOT layer OTel on top. The OR rule gives credit to either axis
// without forcing both. Slice 2 may tighten the predicate per surface if
// operator feedback warrants.
//
// Kept as a method on the snapshot so the scanner-side tally, the
// proposer-side reasoning, and the per-cloud Inventory tab all reference
// the same predicate.
func (s EventSourceInstanceSnapshot) IsInstrumented() bool {
	return s.HasTraceAxis || s.HasLogAxis
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
	// "s3", "alb", "eks", "dynamodb", "ecs". Slice 2 added "rds";
	// slice 3a (v0.88.0) added "s3" and "alb"; slice 3b (v0.89.0)
	// added "eks"; slice 4 (v0.89.6) added "dynamodb"; slice 5
	// (v0.89.10) added "ecs"; future slices add more.
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
