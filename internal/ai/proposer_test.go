// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// proposerTestServer returns an httptest.Server that responds with
// the supplied body for every /v1/messages call. The body is the
// raw JSON the Anthropic API would have returned. Tests can stage
// canned responses without exercising the real model.
func proposerTestServer(t *testing.T, replyBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replyBody)
	}))
}

// anthropicReply wraps a model text response into the Anthropic
// Messages API envelope.
func anthropicReply(modelText string) string {
	body, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": modelText},
		},
		"model": "claude-sonnet-4-6",
		"usage": map[string]any{"input_tokens": 123, "output_tokens": 456},
	})
	return string(body)
}

func proposerServiceForTest(baseURL string) *Service {
	return NewService(Config{
		Enabled:    true,
		APIKey:     "test-key",
		BaseURL:    baseURL,
		MergeModel: "claude-sonnet-4-6",
		MaxTokens:  2048,
	}, zap.NewNop())
}

// TestProposeFromCostSpike_CleanProposal exercises the happy path:
// the model returns a well-formed proposal JSON, the service
// parses it, validation passes.
func TestProposeFromCostSpike_CleanProposal(t *testing.T) {
	reply := anthropicReply(`{
  "declined": false,
  "proposal": {
    "name": "AI: drop container.id from metrics",
    "group_id": "prod-utility-fleet",
    "target_config_id": "cfg-abc",
    "require_approval": true,
    "stages": [
      {"mode":"percentage","percentage":10,"dwell_seconds":600},
      {"mode":"percentage","percentage":100,"dwell_seconds":0}
    ],
    "abort_criteria": {
      "max_drifted_agents": 5,
      "max_error_logs_per_minute": 50,
      "min_dwell_seconds_before_abort": 120
    }
  },
  "reasoning": "Container.id is driving the spike. Drop it from the metrics pipeline; canary first.",
  "evidence": [
    {"kind":"alert","id":"spike-xyz","description":"Cost spike fired at 2026-06-17T14:00Z"},
    {"kind":"configlint","id":"high-cardinality-label"}
  ]
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{
		SpikeID:              "spike-xyz",
		Severity:             "critical",
		Signal:               "metrics",
		BaselineMonthlyUSD:   400,
		PeakMonthlyUSD:       1200,
		PeakPctAboveBaseline: 200,
		StartedAt:            time.Date(2026, 6, 17, 14, 0, 0, 0, time.UTC),
		TopAgents:            []string{"agent-1", "agent-2"},
		TopAttributes:        []string{"container.id"},
		GroupID:              "prod-utility-fleet",
		GroupName:            "Prod Utility Fleet",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Declined, "well-formed proposal should not be declined")
	assert.Equal(t, "prod-utility-fleet", res.Proposal.GroupID)
	assert.Equal(t, "cfg-abc", res.Proposal.TargetConfigID)
	assert.True(t, res.Proposal.RequireApproval, "AI proposals must force require_approval=true")
	assert.Len(t, res.Proposal.Stages, 2)
	assert.Equal(t, 10, res.Proposal.Stages[0].Percentage)
	assert.Equal(t, 600, res.Proposal.Stages[0].DwellSeconds)
	assert.Contains(t, res.Reasoning, "Container.id")
	assert.Len(t, res.Evidence, 2)
	assert.Equal(t, "alert", res.Evidence[0].Kind)
	assert.Equal(t, 123, res.TokensIn)
	assert.Equal(t, 456, res.TokensOut)
}

// TestProposeFromCostSpike_Declined exercises the path where the
// model declines to propose. The service returns a
// non-nil result with Declined=true and no error.
func TestProposeFromCostSpike_Declined(t *testing.T) {
	reply := anthropicReply(`{
  "declined": true,
  "reason": "Spike is under $50/month and attribution is ambiguous."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{
		SpikeID: "spike-small",
		GroupID: "dev-fleet",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Declined)
	assert.Contains(t, res.Reason, "under $50")
	assert.Zero(t, res.Proposal.GroupID, "no proposal when declined")
}

// TestProposeFromCostSpike_InvalidGroupMismatch exercises the
// validator: model returns a group_id that doesn't match the
// context. We reject it rather than let a wrong-group rollout
// reach the engine.
func TestProposeFromCostSpike_InvalidGroupMismatch(t *testing.T) {
	reply := anthropicReply(`{
  "declined": false,
  "proposal": {
    "name": "AI: ...",
    "group_id": "WRONG-GROUP",
    "target_config_id": "cfg-abc",
    "require_approval": true,
    "stages": [{"mode":"percentage","percentage":10,"dwell_seconds":600}],
    "abort_criteria": {}
  },
  "reasoning": "..."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{
		SpikeID: "spike-xyz",
		GroupID: "prod-utility-fleet",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group_id")
}

// TestProposeFromCostSpike_MalformedResponse exercises the path
// where the model returns text that isn't valid JSON. The service
// surfaces the parse error so the bridge daemon can log and skip.
func TestProposeFromCostSpike_MalformedResponse(t *testing.T) {
	reply := anthropicReply(`Sorry, I cannot help with this request. Here's a paragraph instead.`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{
		SpikeID: "spike-xyz",
		GroupID: "prod-utility-fleet",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

// TestProposeFromCostSpike_Disabled exercises the gate: a service
// constructed without an API key short-circuits to ErrDisabled so
// callers don't have to nil-check the service.
func TestProposeFromCostSpike_Disabled(t *testing.T) {
	svc := NewService(Config{Enabled: false}, zap.NewNop())
	_, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{
		SpikeID: "x",
		GroupID: "y",
	})
	require.ErrorIs(t, err, ErrDisabled)
}

// TestProposeFromCostSpike_MissingRequiredFields documents the
// pre-call validation: spike_id and group_id are required.
func TestProposeFromCostSpike_MissingRequiredFields(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	_, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{GroupID: "g"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spike_id")

	_, err = svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{SpikeID: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group_id")
}

// TestBuildProposeUserMessage_IncludesContext confirms the prompt
// builder threads every supplied context field into the message
// body. The system prompt depends on these fields being present
// when the user supplied them.
func TestBuildProposeUserMessage_IncludesContext(t *testing.T) {
	msg := buildProposeUserMessage(CostSpikeContext{
		SpikeID:              "spike-1",
		Severity:             "warn",
		Signal:               "metrics",
		BaselineMonthlyUSD:   500,
		PeakMonthlyUSD:       1500,
		PeakPctAboveBaseline: 200,
		StartedAt:            time.Date(2026, 6, 17, 14, 0, 0, 0, time.UTC),
		GroupID:              "g-1",
		GroupName:            "Prod",
		TopAgents:            []string{"a", "b"},
		TopAttributes:        []string{"container.id"},
		RecentLintFindings:   []string{"high-cardinality-label"},
		RecentRecommendations: []string{
			"Add k8sattributes processor",
		},
	})
	for _, want := range []string{
		"spike-1", "warn", "metrics",
		"g-1", "Prod",
		"container.id",
		"high-cardinality-label",
		"k8sattributes",
		"$500", "$1500",
	} {
		assert.Contains(t, msg, want, "prompt should include %q", want)
	}
	assert.True(t, strings.Contains(msg, "MUST equal"), "prompt should remind the model group_id must equal context")
}

// TestProposeFromCostSpike_RequestsProposerMaxTokens pins #550. The
// v0.79 prompt asks the model to emit a complete inline collector
// YAML per plan step. With the global s.cfg.MaxTokens at 1024 the
// second seeded spike in v0.82 testing truncated mid-config and the
// bridge silently dropped the spike.
//
// v0.82 fixed this by adding a per-call MaxTokens override in
// callOpts and wiring the proposer to use ProposerMaxTokens (4096).
// This test asserts the wire request actually carries that value —
// catches accidental regressions where someone removes the override
// thinking it's redundant. It does NOT reproduce the truncation
// symptom (the fake server doesn't simulate token limits); the real
// safety net for that is stress_live_test.go behind the live build
// tag.
func TestProposeFromCostSpike_RequestsProposerMaxTokens(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicReply(`{"declined":true,"reason":"test"}`))
	}))
	defer srv.Close()

	// Configure the service with the SMALL global default
	// (1024) so we can tell the per-call override is what's
	// landing on the wire and not the global setting bleeding in.
	svc := NewService(Config{
		Enabled:    true,
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		MergeModel: "claude-sonnet-4-6",
		MaxTokens:  1024,
	}, zap.NewNop())

	_, err := svc.ProposeFromCostSpike(context.Background(), CostSpikeContext{
		SpikeID:       "spike-mt",
		Signal:        "metrics",
		GroupID:       "g-1",
		GroupName:     "Prod",
		TopAttributes: []string{"container.id"},
	})
	require.NoError(t, err)

	gotMaxTokens, ok := captured["max_tokens"].(float64)
	require.True(t, ok, "request should carry max_tokens; got %v", captured)
	assert.Equal(t, float64(ProposerMaxTokens), gotMaxTokens,
		"proposer call must use ProposerMaxTokens (%d) not the global s.cfg.MaxTokens (1024); raising the cap is what fixes #550",
		ProposerMaxTokens)
	assert.Equal(t, float64(4096), gotMaxTokens,
		"ProposerMaxTokens itself should stay at 4096 unless we also extend docs/ai-features.md")
}
