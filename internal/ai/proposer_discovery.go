// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DiscoveryScanContext is the v0.85 Stream 2F input to
// ProposeFromDiscoveryScan. Mirrors the cost-spike CostSpikeContext
// posture: a flat struct the handler layer assembles from the typed
// scanner.Result before calling the proposer. Kept here in the ai
// package so the discovery handlers don't pull the AI package into
// their import graph; the converter lives at the call site.
//
// Shape follows the universal-discovery design doc's
// "CloudDiscoveryContext shape" section — provider-typed at the top
// (account_id + regions are AWS-flavored in slice 1; the same fields
// carry GCP project_id / Azure subscription_id in later slices) but
// category-typed underneath so the proposer prompt reasons about
// "compute" and "function" rather than provider-specific resource
// types.
type DiscoveryScanContext struct {
	// Identification. ScanID flows into the audit trail and the
	// recommendation Source.RefID; AccountID doubles as the group_id
	// the model sets on every plan step.
	ScanID    string
	AccountID string

	// Provider — v0.89.48 (#671 Stream 69, GCP discovery slice 1
	// chunk 5) — discriminates the cloud surface the recommendations
	// target. Slice 1 supports "aws" and "gcp"; empty string is
	// treated as "aws" for backward compatibility with v0.89.47 and
	// earlier callers that pre-date the field. When Provider="aws"
	// (or empty), AccountID carries the AWS account ID and
	// ProjectID is empty. When Provider="gcp", ProjectID carries the
	// GCP project ID and AccountID is empty. See
	// docs/proposals/gcp-discovery-slice1.md §9.
	Provider string

	// ProjectID — v0.89.48 (#671 Stream 69, GCP discovery slice 1
	// chunk 5) — populated when Provider="gcp"; carries the GCP
	// project ID the scan walked. Empty for Provider="aws". Used by
	// ScopeID() so the verdict learning loop's scope tuple and the
	// audit payload composition can stay provider-agnostic
	// downstream. The proposer's pre-call validator enforces
	// non-empty ProjectID when Provider="gcp" (mirroring the AWS
	// AccountID enforcement) so the prompt body's scope description
	// has a real value to bind to.
	ProjectID string

	// TenantID — v0.89.53 (#678 Stream 76, Azure discovery slice 1
	// chunk 5) — populated when Provider="azure"; carries the Azure
	// AD tenant ID the scan walked. Empty for Provider="aws" and
	// Provider="gcp". The proposer prompt body cites it alongside
	// SubscriptionID so the model's reasoning can hedge in
	// cross-tenant edge cases the runbook documents. See
	// docs/proposals/azure-discovery-slice1.md §10.
	TenantID string

	// SubscriptionID — v0.89.53 (#678 Stream 76, Azure discovery
	// slice 1 chunk 5) — populated when Provider="azure"; carries
	// the Azure subscription ID the scan walked. Empty for
	// Provider="aws" and Provider="gcp". Used by ScopeID() so the
	// verdict learning loop's scope tuple and the audit payload
	// composition stay provider-agnostic downstream. The proposer's
	// pre-call validator enforces non-empty SubscriptionID when
	// Provider="azure" (mirroring the AWS AccountID + GCP ProjectID
	// enforcement) so the prompt body's scope description has a real
	// value to bind to.
	SubscriptionID string

	// TenancyOCID — v0.89.58 (#685 Stream 83, OCI discovery slice 1
	// chunk 5) — populated when Provider="oci"; carries the Oracle
	// Cloud tenancy OCID the scan walked. Empty for Provider="aws",
	// Provider="gcp", and Provider="azure". Used by ScopeID() so the
	// verdict learning loop's scope tuple and the audit payload
	// composition stay provider-agnostic downstream. The proposer's
	// pre-call validator enforces non-empty TenancyOCID when
	// Provider="oci" (mirroring the AWS AccountID + GCP ProjectID +
	// Azure SubscriptionID enforcement) so the prompt body's scope
	// description has a real value to bind to. See
	// docs/proposals/oci-discovery-slice1.md §10.
	TenancyOCID string

	// UserOCID — v0.89.58 (#685 Stream 83, OCI discovery slice 1
	// chunk 5) — the OCI user identity used to scan the tenancy.
	// Populated when Provider="oci". Surfaced on the context so the
	// prompt builder can include it as evidence in the rendered user
	// message; downstream audit + scope plumbing uses TenancyOCID.
	UserOCID string

	// CompartmentID — v0.89.58 (#685 Stream 83, OCI discovery slice 1
	// chunk 5) — restricts the scan to a specific OCI compartment
	// when set. Empty means the slice 1 default walk: root +
	// first-level children per docs/proposals/oci-discovery-slice1.md
	// §9. Populated when Provider="oci"; ignored for other providers.
	CompartmentID string

	// Regions the scan walked. Slice 1 ships single-entry slices;
	// slice 3 will iterate.
	Regions []string

	// Inventory snapshot — category-typed, not provider-typed. The
	// proposer reasons about which to instrument first based on
	// count + runtime + coverage gap.
	ComputeInstances []ComputeResourceCandidate
	Functions        []FunctionResourceCandidate
	// Databases joins Compute + Functions in slice 2 of the
	// universal-observation arc. RDS-shaped today; Cloud SQL / Azure
	// SQL slot into the same field in later slices since the
	// observability-lever model (PI + EM) generalizes.
	Databases []DatabaseResourceCandidate

	// ObjectStores joins Compute + Functions + Databases in slice
	// 3a of the universal-observation arc (v0.88.0). S3-shaped
	// today; GCS / Azure Blob slot into the same field in later
	// slices. The proposer's single lever for object stores is
	// Server Access Logging — when off, recommend enabling with
	// an operator-chosen target bucket + prefix.
	ObjectStores []ObjectStoreCandidate

	// LoadBalancers joins the inventory list in slice 3a (v0.88.0).
	// ALB / NLB / Gateway LB today; GCLB / Azure LB slot into the
	// same field in later slices. The proposer's single lever for
	// load balancers is Access Logs (writes to an S3 bucket). When
	// off, recommend enabling — prefer naming an existing
	// instrumented bucket from the ObjectStores list as the target
	// rather than asking the operator to invent one.
	LoadBalancers []LoadBalancerCandidate

	// Clusters joins the inventory list in slice 3b (v0.89.0). EKS
	// today; GKE / AKS slot into the same field in later slices.
	// The proposer's instrumented rule for clusters is COMPOSITE —
	// control plane logging on (api + audit minimum) AND an ACTIVE
	// observability addon (adot or amazon-cloudwatch-observability)
	// must BOTH hold. The proposer reasons at cluster level and
	// emits a single plan step per uncovered cluster covering both
	// axes. Squadron NEVER executes eks:UpdateCluster or
	// eks:CreateAddon — read-only invariant.
	Clusters []ClusterCandidate

	// DynamoDBTables joins the inventory list in slice 4 (v0.89.6).
	// DynamoDB today; Cosmos DB / Cloud Bigtable slot into the same
	// field in later slices. The proposer's instrumented rule for
	// DynamoDB is SINGLE-axis: ContributorInsightsStatus must be
	// "ENABLED". This is a deliberate downgrade from EKS slice 3b's
	// composite rule — DynamoDB has exactly one cloud-API-visible
	// observability signal per table. Squadron does NOT detect
	// SDK-side OpenTelemetry or X-Ray instrumentation in application
	// code; tables whose SDK is OTel-wrapped on the client side
	// are reported as uninstrumented (cloud-API-only limitation).
	// Squadron NEVER executes dynamodb:UpdateContributorInsights —
	// read-only invariant.
	DynamoDBTables []DynamoDBTableCandidate

	// ECSClusters joins the inventory list in slice 5 (v0.89.10).
	// ECS today; Cloud Run / AKS container-orchestration surfaces
	// slot into the same field in later slices. The proposer's
	// instrumented rule for ECS is SINGLE-axis:
	// ContainerInsightsStatus must be "enabled" (case-insensitive,
	// against the cluster's settings[name=containerInsights].value).
	// Same posture as the DynamoDB slice 4 downgrade — cluster-level
	// Container Insights is the one strong cloud-API-visible
	// observability signal for ECS. Squadron does NOT detect
	// task-definition-level instrumentation (X-Ray daemon sidecars,
	// ADOT collector sidecars, FireLens log routing); clusters
	// whose task defs ship those sidecars but whose cluster does
	// NOT have Container Insights enabled are reported as
	// uninstrumented (cluster-level scanning limitation). Both
	// Fargate and EC2 launch types covered by the same rule.
	// Squadron NEVER executes ecs:UpdateClusterSettings — read-only
	// invariant.
	ECSClusters []ECSClusterCandidate

	// Coverage assessment, denormalized so the prompt body can
	// reference the totals without recounting. Match the scanner
	// Result fields one-to-one.
	InstrumentedCount   int
	UninstrumentedCount int

	// Optional: customer telemetry-backend preference. When set, the
	// proposer can target the OTel layer's exporter to the named
	// backend. Empty means generic (collector endpoint) — slice 1
	// callers leave this empty until the connect-account wizard
	// grows a backend picker.
	PreferredBackend string

	// AcceptedRecommendations — v0.89.28 (#643 slice 1) — kept on the
	// struct for slice 1 callsite parity, but as of v0.89.36 (#655
	// Stream 53, #531 slice 2 chunk 3) the discovery bridge no
	// longer populates this field directly. The wiring layer now
	// passes the fully-rendered prompt stanza through VerdictBlock
	// below. AcceptedRecommendations remains supported as a
	// fallback: when VerdictBlock is empty AND
	// AcceptedRecommendations is non-empty the prompt builder
	// renders the legacy slice 1 stanza so callers that haven't
	// migrated still produce the v0.89.28 prompt body byte-for-byte.
	AcceptedRecommendations []AcceptedRecommendationExample

	// VerdictBlock — v0.89.36 (#655 Stream 53, #531 slice 2 chunk 3)
	// — the fully-rendered verdict prompt block the wiring layer
	// produced via verdictprompt.Render. Includes both the
	// accepted-PR (StateMerged) and the new negative signal
	// (StateClosedNotMerged / StateOperatorExcluded) stanzas in
	// rejection-first order per docs/proposals/
	// 531-proposer-learning-slice2.md §7.2. Empty string on cold
	// start / opt-out / recency-window empty — the prompt builder
	// drops the block entirely so the cold-start prompt remains
	// byte-for-byte identical to the slice 1 (v0.89.28) output.
	VerdictBlock string
}

// ScopeID — v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5)
// — returns the provider-agnostic scope identifier. For Provider="gcp"
// the value is ProjectID; for Provider="azure" (v0.89.53, #678 Stream
// 76, Azure discovery slice 1 chunk 5) the value is SubscriptionID;
// for Provider="aws" (or empty, the backward-compat default) the
// value is AccountID. Used by the verdict learning loop's scope
// tuple, by audit payload composition, and by the prompt body's scope
// description so the discovery proposer's call sites don't need to
// branch on Provider every time they need the per-scope identifier.
//
// See docs/proposals/gcp-discovery-slice1.md §9 and
// docs/proposals/azure-discovery-slice1.md §10 for the broader design
// of the provider-agnostic scope_id substrate.
func (c *DiscoveryScanContext) ScopeID() string {
	if c == nil {
		return ""
	}
	switch c.Provider {
	case "gcp":
		return c.ProjectID
	case "azure":
		return c.SubscriptionID
	case "oci":
		return c.TenancyOCID
	default: // "aws" (or empty for backward compat)
		return c.AccountID
	}
}

// AcceptedRecommendationExample is the minimal projection over a
// prior accepted PR that the discovery proposer threads into the §6
// prompt block. Lives in the ai package (NOT in internal/proposer/)
// to avoid the circular import that v0.89.17 hit: the proposer
// package imports ai, so ai-package types can be consumed by the
// proposer bridge but not vice versa. v0.89.28 (#643 slice 1).
type AcceptedRecommendationExample struct {
	RecommendationKind string
	PRURL              string
	Branch             string
	MergedAt           time.Time
	MergedBy           string
}

// ComputeResourceCandidate is one EC2-shaped row from the scan that
// the proposer might choose to instrument. Mirrors
// scanner.ComputeInstanceSnapshot but lives in the ai package so the
// ai package doesn't import the scanner package — the handler layer
// converts.
type ComputeResourceCandidate struct {
	ResourceID   string
	InstanceType string
	Region       string
	OSFamily     string
	HasOTel      bool
}

// FunctionResourceCandidate is one Lambda-shaped row from the scan.
// Mirrors scanner.FunctionRuntimeSnapshot, same import-boundary
// reason as ComputeResourceCandidate.
type FunctionResourceCandidate struct {
	ResourceID   string
	Name         string
	Runtime      string
	Region       string
	HasOTelLayer bool
}

// DatabaseResourceCandidate is one RDS-shaped row from the scan.
// Mirrors scanner.DatabaseInstanceSnapshot for the same
// import-boundary reason as the other two candidates.
//
// The proposer treats Performance Insights and Enhanced Monitoring as
// independent levers: each is enabled by a separate IAM permission
// (rds:ModifyDBInstance with a different request shape), so when only
// one is missing the model emits a single-lever plan step; when both
// are missing, two steps. The handler-side validator below enforces
// ResourceID + Engine non-empty so the prompt body's reasoning has
// something to bind to.
type DatabaseResourceCandidate struct {
	ResourceID                 string
	Engine                     string
	EngineVersion              string
	InstanceClass              string
	PerformanceInsightsEnabled bool
	EnhancedMonitoringEnabled  bool
	Region                     string
}

// ObjectStoreCandidate is one S3-shaped row from the scan. Mirrors
// scanner.ObjectStoreSnapshot for the same import-boundary reason as
// the other candidates.
//
// The proposer's single S3 lever is Server Access Logging — when
// false, the recommendation is to enable logging to an
// operator-chosen target bucket and prefix. The target bucket can be
// any bucket in the operator's environment; the proposer surfaces
// `inline_config_snippet` placeholders the operator fills in. The
// candidate's RequestMetricsEnabled is intentionally omitted from
// the prompt input — slice 3a's instrumented rule is single-axis
// (server access logging), and request-metrics is operator-facing
// information only.
type ObjectStoreCandidate struct {
	ResourceID                 string `json:"resource_id"`
	Region                     string `json:"region"`
	ServerAccessLoggingEnabled bool   `json:"server_access_logging_enabled"`
}

// LoadBalancerCandidate is one ALB-shaped row from the scan. Mirrors
// scanner.LoadBalancerSnapshot for the same import-boundary reason
// as the other candidates.
//
// The proposer's single ALB lever is Access Logs (writes to an S3
// bucket). When AccessLogsEnabled is false, the recommendation
// names a target bucket. Cross-reference rule: when the inventory
// contains ObjectStores, the proposer prefers naming a bucket
// Squadron already sees as the target rather than asking the
// operator to invent one. AccessLogsS3Bucket is the currently-
// configured target when logging is enabled — surfaced so the
// proposer can decline to re-recommend on already-on rows.
type LoadBalancerCandidate struct {
	ResourceID         string `json:"resource_id"`
	Name               string `json:"name"`
	Type               string `json:"type"`
	Scheme             string `json:"scheme"`
	AccessLogsEnabled  bool   `json:"access_logs_enabled"`
	AccessLogsS3Bucket string `json:"access_logs_s3_bucket,omitempty"`
	Region             string `json:"region"`
}

// ClusterCandidate is one EKS-shaped row from the scan. Mirrors
// scanner.ClusterSnapshot for the same import-boundary reason as
// the other candidates.
//
// The proposer's instrumented rule for clusters is COMPOSITE.
// Axis 1: ControlPlaneLogging must include BOTH "api" AND "audit".
// Axis 2: AddonNames must contain "adot" OR
// "amazon-cloudwatch-observability". (The dispatch glue at the
// handler layer flattens addons[*].name where status is ACTIVE
// before populating AddonNames here, so the proposer prompt body
// doesn't have to re-implement the status filter.) Both axes must
// hold for a cluster to count as covered; either alone is
// insufficient.
type ClusterCandidate struct {
	ResourceID          string   `json:"resource_id"`
	Name                string   `json:"name"`
	KubernetesVersion   string   `json:"kubernetes_version"`
	ControlPlaneLogging []string `json:"control_plane_logging"`
	AddonNames          []string `json:"addon_names"`
	Region              string   `json:"region"`
}

// DynamoDBTableCandidate is one DynamoDB-shaped row from the scan.
// Mirrors scanner.DynamoDBTableSnapshot for the same import-boundary
// reason as the other candidates.
//
// The proposer's single DynamoDB lever is Contributor Insights
// enablement. ContributorInsightsStatus carries the four AWS API
// enum values ("ENABLED", "DISABLED", "ENABLING", "DISABLING",
// "FAILED") plus the scanner's "UNKNOWN" sentinel surfaced when
// the operator's policy granted dynamodb:DescribeTable but not
// dynamodb:DescribeContributorInsights. The instrumented rule is
// "ENABLED" only — every other value (including UNKNOWN) counts
// as uninstrumented.
//
// SDK-side limitation (honest restatement at the import boundary):
// Squadron detects resource-side Contributor Insights via the
// DescribeContributorInsights API. Squadron does not detect
// SDK-side OpenTelemetry or X-Ray instrumentation in your
// application code. The proposer prompt repeats this limitation
// so the model can hedge in its reasoning when the operator's
// preferred backend implies SDK-side instrumentation is likely
// already present.
type DynamoDBTableCandidate struct {
	ResourceID                string `json:"resource_id"`
	Name                      string `json:"name"`
	BillingMode               string `json:"billing_mode,omitempty"`
	ContributorInsightsStatus string `json:"contributor_insights_status"`
	Region                    string `json:"region"`
}

// ECSClusterCandidate is one ECS-shaped row from the scan. Mirrors
// scanner.ECSClusterSnapshot for the same import-boundary reason as
// the other candidates.
//
// The proposer's single ECS lever is cluster-level CloudWatch
// Container Insights enablement. ContainerInsightsStatus carries
// the three AWS-side values ("enabled" / "disabled" / "enhanced")
// plus the scanner's "UNKNOWN" sentinel surfaced when the
// DescribeClusters response did not return the containerInsights
// setting. The instrumented rule is "enabled" only
// (case-insensitive) — every other value (including UNKNOWN and
// "enhanced") counts as uninstrumented.
//
// Task-definition-level limitation (honest restatement at the
// import boundary): Squadron detects cluster-level Container
// Insights via the DescribeClusters API. Squadron does not detect
// task-definition-level instrumentation — X-Ray daemon sidecars,
// ADOT collector sidecars, or FireLens log routing in your task
// definitions. If your task defs include those sidecars but the
// cluster does not have Container Insights enabled, Squadron will
// report the cluster as uninstrumented — this is a known
// limitation of cluster-level scanning. A future slice can extend
// the rule to inspect task definitions if operators request it.
//
// Both Fargate and EC2 launch types are covered by the same
// per-cluster rule.
//
// The task / service counts are surfaced so the prompt body's
// per-cluster reasoning can highlight high-traffic clusters when
// surfacing the recommendation (a high RunningTasksCount with
// disabled Container Insights is the cluster the proposer flags
// first).
type ECSClusterCandidate struct {
	ARN                               string `json:"arn"`
	Name                              string `json:"name"`
	Status                            string `json:"status"`
	ContainerInsightsStatus           string `json:"container_insights_status"`
	RegisteredContainerInstancesCount int    `json:"registered_container_instances_count,omitempty"`
	RunningTasksCount                 int    `json:"running_tasks_count,omitempty"`
	PendingTasksCount                 int    `json:"pending_tasks_count,omitempty"`
	ActiveServicesCount               int    `json:"active_services_count,omitempty"`
	Region                            string `json:"region"`
}

// ProposeFromDiscoveryScan is the v0.85 sibling of
// ProposeFromCostSpike. Same proposer engine, different entry point.
// The model receives a Squadron-discovered AWS inventory snapshot and
// emits a multi-step plan whose inline_config_snippet per step is
// Terraform the operator runs through their IaC pipeline. Squadron
// does NOT execute the Terraform — the system prompt states this
// explicitly so the model never suggests an auto-apply path.
//
// Output shape: plan-kind ONLY. Discovery is always staged so the
// operator can observe between batches. We surface a clear validation
// error if the model returns kind=rollout from this entry point — the
// cost-spike path supports both kinds; the discovery path does not.
//
// Errors are returned for service-level problems (disabled, HTTP
// failure, malformed response that can't be salvaged, model returned
// the wrong kind, or model returned empty Terraform). The proposer
// declining to propose is NOT an error; it's a normal
// ProposalResult with Declined=true.
func (s *Service) ProposeFromDiscoveryScan(ctx context.Context, in *DiscoveryScanContext) (*ProposalResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if in == nil {
		return nil, errors.New("discovery scan context is required")
	}
	if in.ScanID == "" {
		return nil, errors.New("scan_id is required")
	}
	// v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5) —
	// provider-aware required-scope check. Provider="gcp" requires
	// ProjectID; v0.89.53 (#678 Stream 76, Azure discovery slice 1
	// chunk 5) adds Provider="azure" requires SubscriptionID;
	// Provider="aws" (or empty for backward compat) requires
	// AccountID. The ScopeID() helper folds all three into one
	// assertion so downstream code paths can stay provider-agnostic.
	switch in.Provider {
	case "gcp":
		if in.ProjectID == "" {
			return nil, errors.New("project_id is required when provider=gcp")
		}
	case "azure":
		if in.SubscriptionID == "" {
			return nil, errors.New("subscription_id is required when provider=azure")
		}
	case "oci":
		if in.TenancyOCID == "" {
			return nil, errors.New("tenancy_ocid is required when provider=oci")
		}
	default:
		if in.AccountID == "" {
			return nil, errors.New("account_id is required")
		}
	}
	if err := validateDatabaseCandidates(in.Databases); err != nil {
		return nil, fmt.Errorf("discovery scan context: %w", err)
	}
	if err := validateObjectStoreCandidates(in.ObjectStores); err != nil {
		return nil, fmt.Errorf("discovery scan context: %w", err)
	}
	if err := validateLoadBalancerCandidates(in.LoadBalancers); err != nil {
		return nil, fmt.Errorf("discovery scan context: %w", err)
	}
	if err := validateClusterCandidates(in.Clusters); err != nil {
		return nil, fmt.Errorf("discovery scan context: %w", err)
	}
	if err := validateDynamoDBTableCandidates(in.DynamoDBTables); err != nil {
		return nil, fmt.Errorf("discovery scan context: %w", err)
	}
	if err := validateECSClusterCandidates(in.ECSClusters); err != nil {
		return nil, fmt.Errorf("discovery scan context: %w", err)
	}

	resp, err := s.callMessages(ctx, callOpts{
		Model:  s.cfg.MergeModel,
		System: proposeFromDiscoveryScanSystem,
		// Reuse the v0.82 proposer cap. Discovery plans emit Terraform
		// per step (typically denser than collector YAML) so the
		// 4096-token headroom is at least as important here as for the
		// cost-spike plan kind. Same constant per v0.82's #550 fix.
		MaxTokens: ProposerMaxTokens,
		UserText:  buildDiscoveryUserMessage(*in),
	})
	if err != nil {
		return nil, fmt.Errorf("propose from discovery scan: %w", err)
	}

	// Parse the JSON block. Mirrors ProposeFromCostSpike's parsed
	// shape — same fields, same extractJSONBlock helper. We expect
	// kind=plan; the handler validates and rejects rollout-kind
	// explicitly below.
	type parsed struct {
		Declined  bool                   `json:"declined"`
		Reason    string                 `json:"reason"`
		Kind      ProposalKind           `json:"kind"`
		Proposal  RolloutInputCandidate  `json:"proposal"`
		Plan      PlanCandidate          `json:"plan"`
		Reasoning string                 `json:"reasoning"`
		Evidence  []EvidenceRefCandidate `json:"evidence"`
	}
	raw := extractJSONBlock(resp.Text)
	var p parsed
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("propose from discovery scan: model response was not valid JSON: %w (raw=%s)",
			err, truncateString(resp.Text, 400))
	}

	result := &ProposalResult{
		Declined:  p.Declined,
		Reason:    strings.TrimSpace(p.Reason),
		Kind:      p.Kind,
		Reasoning: strings.TrimSpace(p.Reasoning),
		Evidence:  p.Evidence,
		Model:     resp.Model,
		TokensIn:  resp.TokensIn,
		TokensOut: resp.TokensOut,
	}

	if p.Declined {
		// Declined is a normal outcome — the model said no productive
		// instrumentation plan exists for this scan. The handler
		// passes the reason through to the UI without an error.
		return result, nil
	}

	// Plan-kind ONLY for discovery. Empty Kind defaults to rollout
	// in the cost-spike path for backwards compat; here we treat it
	// as a violation. The discovery prompt explicitly tells the
	// model to set kind="plan"; if it didn't, the response is bad.
	kind := p.Kind
	if kind == "" {
		// Allow the empty default only when the plan body is
		// present — some models emit kind=plan implicitly by
		// returning a plan field. Without a plan body, treat as
		// the rollout default and reject below.
		if len(p.Plan.Steps) > 0 {
			kind = ProposalKindPlan
		} else {
			kind = ProposalKindRollout
		}
	}
	if kind != ProposalKindPlan {
		return nil, fmt.Errorf("propose from discovery scan: model returned kind %q; discovery is plan-only", kind)
	}
	result.Kind = ProposalKindPlan
	result.Plan = p.Plan

	// v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5) —
	// provider-aware plan-step group_id check. Slice 1's discovery
	// pipeline uses the provider-agnostic scope_id (account_id for AWS,
	// project_id for GCP) as the group identifier so the per-step
	// validation has one rule, not two.
	if err := validateDiscoveryPlan(p.Plan, in.ScopeID()); err != nil {
		return nil, fmt.Errorf("propose from discovery scan: model returned an invalid plan: %w", err)
	}
	return result, nil
}

// validateDatabaseCandidates is the pre-call validator for the
// DatabaseResourceCandidate rows the caller assembled from the
// scan. Each row must carry a non-empty ResourceID and Engine — the
// proposer prompt cites them back via the evidence array, and an
// empty value would surface in the model's reasoning as "the row
// with empty ARN" which the operator can't act on. Cheap to check
// here; the scanner already enforces ResourceID non-empty on the
// snapshot side, but the candidate types are public and the handler
// converter is fan-in code that's easy to mis-wire — better to fail
// loudly in the proposer call than silently in the prompt body.
func validateDatabaseCandidates(dbs []DatabaseResourceCandidate) error {
	for i, d := range dbs {
		if strings.TrimSpace(d.ResourceID) == "" {
			return fmt.Errorf("databases[%d]: resource_id is required", i)
		}
		if strings.TrimSpace(d.Engine) == "" {
			return fmt.Errorf("databases[%d]: engine is required", i)
		}
	}
	return nil
}

// validateObjectStoreCandidates mirrors validateDatabaseCandidates
// for the slice 3a (v0.88.0) S3 candidate type. Each row must carry
// a non-empty ResourceID (the bucket name) and Region (the bucket's
// home region) — the proposer cites them back via the evidence
// array, and empty values surface in the model's reasoning as
// unactionable rows.
func validateObjectStoreCandidates(stores []ObjectStoreCandidate) error {
	for i, o := range stores {
		if strings.TrimSpace(o.ResourceID) == "" {
			return fmt.Errorf("object_stores[%d]: resource_id is required", i)
		}
		if strings.TrimSpace(o.Region) == "" {
			return fmt.Errorf("object_stores[%d]: region is required", i)
		}
	}
	return nil
}

// validateLoadBalancerCandidates mirrors validateDatabaseCandidates
// for the slice 3a load-balancer candidate type. ResourceID (the
// ARN) and Name + Type are required so the prompt body's reasoning
// has something to bind to; Scheme + Region are denormalized for
// completeness but not enforced (an empty Scheme just renders as
// "scheme=" in the prompt body, which the model handles gracefully).
func validateLoadBalancerCandidates(lbs []LoadBalancerCandidate) error {
	for i, l := range lbs {
		if strings.TrimSpace(l.ResourceID) == "" {
			return fmt.Errorf("load_balancers[%d]: resource_id is required", i)
		}
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("load_balancers[%d]: name is required", i)
		}
		if strings.TrimSpace(l.Type) == "" {
			return fmt.Errorf("load_balancers[%d]: type is required", i)
		}
	}
	return nil
}

// validateClusterCandidates mirrors validateDatabaseCandidates for
// the slice 3b (v0.89.0) EKS candidate type. Each row must carry a
// non-empty ResourceID (the cluster ARN) and Name — the proposer
// cites them back via the evidence array. KubernetesVersion and the
// two axis fields (ControlPlaneLogging, AddonNames) are not
// enforced; an empty slice on either is a valid uncovered signal
// the proposer reasons about directly.
func validateClusterCandidates(cs []ClusterCandidate) error {
	for i, c := range cs {
		if strings.TrimSpace(c.ResourceID) == "" {
			return fmt.Errorf("clusters[%d]: resource_id is required", i)
		}
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("clusters[%d]: name is required", i)
		}
	}
	return nil
}

// validateDynamoDBTableCandidates mirrors validateClusterCandidates
// for the slice 4 (v0.89.6) DynamoDB candidate type. Each row must
// carry a non-empty ResourceID (the table ARN) and Name. The
// ContributorInsightsStatus field is NOT enforced because the
// "uncovered" signal IS an empty-or-non-ENABLED status; the
// validator only enforces identifier fields.
func validateDynamoDBTableCandidates(ts []DynamoDBTableCandidate) error {
	for i, t := range ts {
		if strings.TrimSpace(t.ResourceID) == "" {
			return fmt.Errorf("dynamodb_tables[%d]: resource_id is required", i)
		}
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("dynamodb_tables[%d]: name is required", i)
		}
	}
	return nil
}

// validateECSClusterCandidates mirrors validateDynamoDBTableCandidates
// for the slice 5 (v0.89.10) ECS cluster candidate type. Each row
// must carry a non-empty ARN and Name. The ContainerInsightsStatus
// field is NOT enforced because the "uncovered" signal IS an
// empty-or-non-"enabled" status; the validator only enforces
// identifier fields.
func validateECSClusterCandidates(cs []ECSClusterCandidate) error {
	for i, c := range cs {
		if strings.TrimSpace(c.ARN) == "" {
			return fmt.Errorf("ecs_clusters[%d]: arn is required", i)
		}
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("ecs_clusters[%d]: name is required", i)
		}
	}
	return nil
}

// validateDiscoveryPlan is the discovery-side smoke test on the
// model's plan candidate. Differs from validatePlan only in the
// expected group_id semantics — discovery uses the provider-agnostic
// scope_id as the group_id (account_id for AWS, project_id for GCP,
// per docs/proposals/gcp-discovery-slice1.md §9) — and in the
// no-empty-Terraform requirement (the cost-spike validator already
// enforces inline_config_snippet non-empty for YAML; the discovery
// handler relies on the same check for HCL). Kept as a separate
// function so future divergence (e.g. Terraform-syntax preflight)
// can land here without touching the cost-spike path.
//
// v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5) renamed
// the second parameter from expectedAccountID to expectedScopeID to
// match the provider-agnostic substrate; AWS callers pass
// AccountID, GCP callers pass ProjectID, and the proposer call site
// already routes through DiscoveryScanContext.ScopeID().
func validateDiscoveryPlan(p PlanCandidate, expectedScopeID string) error {
	if len(p.Steps) == 0 {
		return errors.New("plan has no steps")
	}
	if len(p.Steps) > 10 {
		return fmt.Errorf("plan has %d steps (max 10)", len(p.Steps))
	}
	for i, step := range p.Steps {
		if step.GroupID == "" {
			return fmt.Errorf("plan step %d missing group_id", i)
		}
		if step.GroupID != expectedScopeID {
			return fmt.Errorf("plan step %d group_id %q does not match context scope_id %q",
				i, step.GroupID, expectedScopeID)
		}
		if strings.TrimSpace(step.InlineConfigSnippet) == "" {
			return fmt.Errorf("plan step %d missing inline_config_snippet (Terraform)", i)
		}
	}
	return nil
}
