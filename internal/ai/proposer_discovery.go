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
