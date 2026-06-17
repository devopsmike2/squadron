// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/services"
)

func aiRollout() *services.Rollout {
	return &services.Rollout{
		ID:                "rollout-1",
		Name:              "AI: drop container.id from metrics",
		State:             services.RolloutStatePendingApproval,
		ProposedBy:        services.RolloutProposedByAI,
		ProposalReasoning: "Container.id is the dominant attribute. Drop it from the metrics pipeline; canary first.",
		EvidenceRefs: []services.EvidenceRef{
			{Kind: "alert", ID: "spike-1", Description: "Cost spike fired at 14:00Z"},
			{Kind: "configlint", ID: "high-cardinality-label"},
			{Kind: "recommendation", URL: "https://squadron.example/recs/123", Description: "Add k8sattributes processor"},
			{Kind: "audit_event", ID: "evt-4"}, // fourth ref — should be dropped by topEvidence
		},
	}
}

// TestAIProposalSlackBlocks_HappyPath verifies the rendered blocks
// include header, AI-origin context, reasoning, and evidence with
// a Slack-style link for refs that have a URL.
func TestAIProposalSlackBlocks_HappyPath(t *testing.T) {
	r := aiRollout()
	blocks := aiProposalSlackBlocks(r, "created")
	require.GreaterOrEqual(t, len(blocks), 4)

	// Header block
	header := blocks[0]
	assert.Equal(t, "header", header["type"])
	headerText := header["text"].(map[string]any)["text"].(string)
	assert.Contains(t, headerText, "AI proposed")
	assert.Contains(t, headerText, "AI: drop container.id from metrics")

	// Context block names AI as the origin and includes transition.
	ctx := blocks[1]
	assert.Equal(t, "context", ctx["type"])
	ctxText := ctx["elements"].([]map[string]any)[0]["text"].(string)
	assert.Contains(t, ctxText, "AI proposer")
	assert.Contains(t, ctxText, "created")

	// Find reasoning + evidence sections
	var reasoningBlock, evidenceBlock map[string]any
	for _, b := range blocks[2:] {
		text, ok := b["text"].(map[string]any)
		if !ok {
			continue
		}
		body := text["text"].(string)
		if strings.HasPrefix(body, "*Reasoning*") {
			reasoningBlock = b
		}
		if strings.HasPrefix(body, "*Evidence*") {
			evidenceBlock = b
		}
	}
	require.NotNil(t, reasoningBlock)
	require.NotNil(t, evidenceBlock)

	reasoningText := reasoningBlock["text"].(map[string]any)["text"].(string)
	assert.Contains(t, reasoningText, "Container.id is the dominant attribute")

	evidenceText := evidenceBlock["text"].(map[string]any)["text"].(string)
	// First three refs appear; the fourth is dropped by topEvidence(3).
	assert.Contains(t, evidenceText, "Cost spike fired at 14:00Z")
	assert.Contains(t, evidenceText, "configlint:high-cardinality-label")
	assert.Contains(t, evidenceText, "<https://squadron.example/recs/123|Add k8sattributes processor>")
	assert.NotContains(t, evidenceText, "evt-4", "fourth evidence ref should be omitted by topEvidence cap")
}

// TestAIProposalSlackBlocks_NoReasoningOrEvidence renders cleanly
// when the proposer returned a sparse result (header + context
// only). The blocks should still be valid Slack Block Kit.
func TestAIProposalSlackBlocks_NoReasoningOrEvidence(t *testing.T) {
	r := &services.Rollout{
		ID:         "rollout-2",
		Name:       "AI: sparse proposal",
		State:      services.RolloutStatePending,
		ProposedBy: services.RolloutProposedByAI,
	}
	blocks := aiProposalSlackBlocks(r, "started")
	require.Len(t, blocks, 2, "no reasoning + no evidence yields header + context only")
	assert.Equal(t, "header", blocks[0]["type"])
	assert.Equal(t, "context", blocks[1]["type"])
}

// TestTopEvidence verifies the cap helper.
func TestTopEvidence(t *testing.T) {
	in := []services.EvidenceRef{{Kind: "a"}, {Kind: "b"}, {Kind: "c"}, {Kind: "d"}}
	out := topEvidence(in, 3)
	require.Len(t, out, 3)
	assert.Equal(t, "a", out[0].Kind)
	assert.Equal(t, "c", out[2].Kind)

	assert.Len(t, topEvidence(in, 10), 4, "no truncation when cap exceeds length")
	assert.Empty(t, topEvidence(nil, 3))
}

// TestFormatEvidenceLine verifies the per-ref Slack mrkdwn shape.
func TestFormatEvidenceLine(t *testing.T) {
	assert.Equal(t, "• <https://x|My alert>", formatEvidenceLine(services.EvidenceRef{
		URL:         "https://x",
		Description: "My alert",
	}))
	assert.Equal(t, "• alert:spike-1", formatEvidenceLine(services.EvidenceRef{
		Kind: "alert",
		ID:   "spike-1",
	}))
	assert.Equal(t, "• Cost spike fired", formatEvidenceLine(services.EvidenceRef{
		Description: "Cost spike fired",
	}))
	assert.Equal(t, "• evidence", formatEvidenceLine(services.EvidenceRef{}))
}

// TestTruncate verifies the cap helper.
func TestTruncate(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 50))
	assert.Equal(t, "exactly", truncate("exactly", 7))
	assert.Equal(t, "too lon…", truncate("too long for limit", 7))
}

// TestAIProposalSlackBlocks_SerializesAsValidJSON guards against
// any future change that introduces a non-JSON-serializable value
// into the block shape. The webhook payload marshals to JSON so a
// regression here would break delivery silently.
func TestAIProposalSlackBlocks_SerializesAsValidJSON(t *testing.T) {
	blocks := aiProposalSlackBlocks(aiRollout(), "created")
	body, err := json.Marshal(blocks)
	require.NoError(t, err)
	assert.Contains(t, string(body), "AI proposer")
}
