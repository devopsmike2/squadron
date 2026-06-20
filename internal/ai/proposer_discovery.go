// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	if in.AccountID == "" {
		return nil, errors.New("account_id is required")
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

	if err := validateDiscoveryPlan(p.Plan, in.AccountID); err != nil {
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

// validateDiscoveryPlan is the discovery-side smoke test on the
// model's plan candidate. Differs from validatePlan only in the
// expected group_id semantics — discovery uses account_id as the
// group_id (per the design doc's "account_id is the primary key"
// posture) — and in the no-empty-Terraform requirement (the
// cost-spike validator already enforces inline_config_snippet
// non-empty for YAML; the discovery handler relies on the same check
// for HCL). Kept as a separate function so future divergence
// (e.g. Terraform-syntax preflight) can land here without touching
// the cost-spike path.
func validateDiscoveryPlan(p PlanCandidate, expectedAccountID string) error {
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
		if step.GroupID != expectedAccountID {
			return fmt.Errorf("plan step %d group_id %q does not match context account_id %q",
				i, step.GroupID, expectedAccountID)
		}
		if strings.TrimSpace(step.InlineConfigSnippet) == "" {
			return fmt.Errorf("plan step %d missing inline_config_snippet (Terraform)", i)
		}
	}
	return nil
}
