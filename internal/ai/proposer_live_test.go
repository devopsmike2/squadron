// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build live

// Live regression tests for the proposer's interaction with the real
// Anthropic Messages API. Excluded from the default build; run with:
//
//	ANTHROPIC_API_KEY=sk-ant-... go test -tags=live \
//	    -run TestProposeFromCostSpike_Live \
//	    ./internal/ai/...
//
// Cost: a single call to the merge-grade model. Roughly USD $0.01 at
// v0.82 token sizes.

package ai

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestProposeFromCostSpike_Live_PlanKindFitsInMaxTokens pins #550.
//
// The v0.81.x proposer truncated mid-JSON on plan-kind responses
// because the global s.cfg.MaxTokens of 1024 was too tight for
// 2-step plans each carrying a full inline collector YAML in
// inline_config_snippet (the v0.78 contract). The bridge logged a
// warning and silently dropped the spike.
//
// v0.82 fixed this by raising the per-call cap to ProposerMaxTokens
// (4096) via the callOpts.MaxTokens override. This test exercises
// the fix against the real model:
//
//  1. Configures the service with the SMALL 1024 default (the same
//     value that previously caused the truncation) so we can prove
//     the per-call override is doing the work.
//  2. Builds a context that the v0.79 decision framework will steer
//     toward a plan-kind response (2 independent high-cardinality
//     attributes, baseline tracked, fleet sized so the
//     decision-framework picks "stage as plan" over "single
//     rollout"). The model is allowed to disagree — we accept
//     either kind — but the parse must succeed.
//  3. Asserts ProposeFromCostSpike returns no error and the parsed
//     ProposalResult is structurally complete. A truncated response
//     would fail the JSON parse and surface as a non-nil err.
//
// If this test starts failing again, either:
//   - the corpus grew enough that 4096 is the new 1024 (re-measure
//     and raise ProposerMaxTokens, document the trade-off), or
//   - the model output format changed in a way the prompt no longer
//     anticipates (regression in the v0.79 worked example).
func TestProposeFromCostSpike_Live_PlanKindFitsInMaxTokens(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; live test requires a real key")
	}

	// Service configured with the SAME small global default that
	// caused #550. The proposer's per-call override is what should
	// rescue this — if we accidentally remove that override the
	// test will reproduce the original truncation.
	svc := NewService(Config{
		Enabled:      true,
		APIKey:       key,
		ExplainModel: DefaultExplainModel,
		MergeModel:   DefaultMergeModel,
		MaxTokens:    1024,
	}, zap.NewNop())

	// Context shaped to encourage a plan-kind response per the
	// v0.79 decision framework: two independent high-cardinality
	// attributes is the canonical "stage as plan" signal.
	ctx := CostSpikeContext{
		SpikeID:              "spike-#550-live-repro",
		Signal:               "metrics",
		Severity:             "critical",
		BaselineMonthlyUSD:   400.0,
		PeakMonthlyUSD:       1648.0,
		PeakPctAboveBaseline: 312.0,
		GroupID:              "demo-web-prod",
		GroupName:            "Demo Web Prod",
		TopAttributes:        []string{"container.id", "k8s.pod.uid"},
		TopAgents:            []string{"demo-collector-1"},
		RecentLintFindings:   []string{"high-cardinality-label"},
	}

	result, err := svc.ProposeFromCostSpike(context.Background(), ctx)

	// Truncation symptom is `unexpected end of JSON input` raised
	// from ProposeFromCostSpike's parse step. If that comes back we
	// reproduced #550.
	require.NoError(t, err, "ProposeFromCostSpike should parse cleanly — #550 regression if you see truncation here")

	// Either kind is fine; what matters is the response wasn't
	// truncated. Validate the kind-specific shape.
	if result.Declined {
		t.Logf("model declined: %s — not a #550 reproduction but worth eyeballing", result.Reason)
		return
	}
	switch result.Kind {
	case ProposalKindPlan:
		require.GreaterOrEqual(t, len(result.Plan.Steps), 1,
			"kind=plan must carry at least one step")
		for i, step := range result.Plan.Steps {
			assert.NotEmpty(t, step.Name, "step %d name", i)
			assert.NotEmpty(t, step.InlineConfigSnippet,
				"step %d inline_config_snippet — required by v0.78 contract", i)
			assert.GreaterOrEqual(t, len(step.Stages), 1,
				"step %d stages", i)
			// Belt-and-suspenders: the response should not
			// contain the truncation sentinel we observed in #550
			// (mid-token cutoff inside the inline_config_snippet).
			assert.False(t, strings.HasSuffix(step.InlineConfigSnippet, "…"),
				"step %d inline_config_snippet looks truncated", i)
		}
		t.Logf("plan-kind response: %d steps, model=%s, tokens_in=%d tokens_out=%d",
			len(result.Plan.Steps), result.Model, result.TokensIn, result.TokensOut)
	case ProposalKindRollout, "":
		assert.NotEmpty(t, result.Proposal.Name, "kind=rollout must carry a Proposal payload")
		assert.GreaterOrEqual(t, len(result.Proposal.Stages), 1)
		t.Logf("rollout-kind response (model preferred single rollout): model=%s tokens_in=%d tokens_out=%d",
			result.Model, result.TokensIn, result.TokensOut)
	default:
		t.Fatalf("unexpected kind: %q", result.Kind)
	}
}

