// BUG #259 fix — chunk-by-tier discovery proposal.
//
// ProposeFromDiscoveryScan originally built ONE Anthropic call covering every
// uninstrumented resource across every tier. On a realistic multi-tier account
// that single call (a) exceeded the HTTP timeout and (b) returned invalid JSON —
// truncated at max_tokens, or with a bad escape in a large inline_config_snippet.
// The model already batches WITHIN a tier (one plan step for "instrument 2 Lambda
// functions"), so response size scales with the number of TIERS. Fanning out one
// call per tier keeps every prompt and response small and valid; the per-tier
// plans are merged back into one ProposalResult.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

const (
	discoveryChunkTierThreshold     = 3
	discoveryChunkResourceThreshold = 12
)

func discoveryInventorySize(in *DiscoveryScanContext) (tiers, resources int) {
	add := func(n int) {
		if n > 0 {
			tiers++
			resources += n
		}
	}
	add(len(in.ComputeInstances))
	add(len(in.Functions))
	add(len(in.Databases))
	add(len(in.ObjectStores))
	add(len(in.LoadBalancers))
	add(len(in.Clusters))
	add(len(in.DynamoDBTables))
	add(len(in.ECSClusters))
	add(len(in.EventSources))
	return tiers, resources
}

func splitDiscoveryByTier(in *DiscoveryScanContext) []*DiscoveryScanContext {
	base := *in
	base.ComputeInstances = nil
	base.Functions = nil
	base.Databases = nil
	base.ObjectStores = nil
	base.LoadBalancers = nil
	base.Clusters = nil
	base.DynamoDBTables = nil
	base.ECSClusters = nil
	base.EventSources = nil

	var out []*DiscoveryScanContext
	if len(in.ComputeInstances) > 0 {
		c := base
		c.ComputeInstances = in.ComputeInstances
		out = append(out, &c)
	}
	if len(in.Functions) > 0 {
		c := base
		c.Functions = in.Functions
		out = append(out, &c)
	}
	if len(in.Databases) > 0 {
		c := base
		c.Databases = in.Databases
		out = append(out, &c)
	}
	if len(in.ObjectStores) > 0 {
		c := base
		c.ObjectStores = in.ObjectStores
		out = append(out, &c)
	}
	if len(in.LoadBalancers) > 0 {
		c := base
		c.LoadBalancers = in.LoadBalancers
		out = append(out, &c)
	}
	if len(in.Clusters) > 0 {
		c := base
		c.Clusters = in.Clusters
		out = append(out, &c)
	}
	if len(in.DynamoDBTables) > 0 {
		c := base
		c.DynamoDBTables = in.DynamoDBTables
		out = append(out, &c)
	}
	if len(in.ECSClusters) > 0 {
		c := base
		c.ECSClusters = in.ECSClusters
		out = append(out, &c)
	}
	if len(in.EventSources) > 0 {
		c := base
		c.EventSources = in.EventSources
		out = append(out, &c)
	}
	return out
}

func (s *Service) runDiscoveryChunks(ctx context.Context, in *DiscoveryScanContext) (*ProposalResult, error) {
	tiers, resources := discoveryInventorySize(in)
	if tiers < discoveryChunkTierThreshold && resources <= discoveryChunkResourceThreshold {
		return s.proposeDiscoveryChunk(ctx, in)
	}
	chunks := splitDiscoveryByTier(in)
	if len(chunks) <= 1 {
		return s.proposeDiscoveryChunk(ctx, in)
	}
	results := make([]*ProposalResult, 0, len(chunks))
	var lastErr error
	for _, c := range chunks {
		r, err := s.proposeDiscoveryChunk(ctx, c)
		if err != nil {
			lastErr = err
			s.logger.Warn("discovery proposer: tier chunk failed, continuing with the rest",
				zap.String("scan_id", in.ScanID), zap.Error(err))
			continue
		}
		results = append(results, r)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("propose from discovery scan: all %d tier chunks failed; last error: %w", len(chunks), lastErr)
	}
	return mergeDiscoveryResults(results), nil
}

func mergeDiscoveryResults(results []*ProposalResult) *ProposalResult {
	merged := &ProposalResult{Kind: ProposalKindPlan, Declined: true}
	var reasonings, reasons []string
	for _, r := range results {
		if r == nil {
			continue
		}
		merged.TokensIn += r.TokensIn
		merged.TokensOut += r.TokensOut
		if merged.Model == "" {
			merged.Model = r.Model
		}
		merged.Plan.Steps = append(merged.Plan.Steps, r.Plan.Steps...)
		merged.Evidence = append(merged.Evidence, r.Evidence...)
		if !r.Declined {
			merged.Declined = false
		}
		if t := strings.TrimSpace(r.Reasoning); t != "" {
			reasonings = append(reasonings, t)
		}
		if r.Declined {
			if t := strings.TrimSpace(r.Reason); t != "" {
				reasons = append(reasons, t)
			}
		}
	}
	merged.Reasoning = strings.Join(reasonings, "\n\n")
	if merged.Declined {
		merged.Reason = strings.Join(reasons, " ")
	}
	return merged
}

func (s *Service) proposeDiscoveryChunk(ctx context.Context, in *DiscoveryScanContext) (*ProposalResult, error) {
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
