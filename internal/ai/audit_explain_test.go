// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExplainAuditEvent_HappyPath(t *testing.T) {
	fake := newFake(t, "Squadron approved a rollout to the web-prod group at 14:23 UTC.")
	srv := fake.start()
	defer srv.Close()

	s := mkService(t, srv.URL)
	result, err := s.ExplainAuditEvent(context.Background(), ExplainAuditEventInput{
		EventID:    "evt-123",
		Timestamp:  time.Date(2026, 6, 18, 14, 23, 0, 0, time.UTC),
		Actor:      "operator:alice@example.com",
		EventType:  "rollout.approved",
		TargetType: "rollout",
		TargetID:   "rollout-xyz",
		Action:     "approved",
		Payload: map[string]any{
			"approval_notes": "looks good, the canary held",
		},
		Context: map[string]string{
			"rollout.name":  "web-prod-canary",
			"rollout.state": "running",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Explanation, "web-prod")
	assert.Equal(t, "claude-haiku-4-5-20251001", result.Model)
	assert.Equal(t, 42, result.TokensIn)
	assert.Equal(t, 17, result.TokensOut)

	// The system prompt should contain the explain-audit rules.
	system, _ := fake.lastBodyJSON["system"].(string)
	assert.Contains(t, system, "audit log entry")
	assert.Contains(t, system, "Never use hyphens")
}

func TestExplainAuditEvent_DisabledReturnsError(t *testing.T) {
	s := NewService(Config{Enabled: false}, nil)
	_, err := s.ExplainAuditEvent(context.Background(), ExplainAuditEventInput{
		EventID:   "evt",
		EventType: "x.y",
	})
	require.ErrorIs(t, err, ErrDisabled)
}

func TestExplainAuditEvent_RedactsPayloadBeforePromptingModel(t *testing.T) {
	fake := newFake(t, "Some explanation.")
	srv := fake.start()
	defer srv.Close()

	s := mkService(t, srv.URL)
	result, err := s.ExplainAuditEvent(context.Background(), ExplainAuditEventInput{
		EventID:    "evt-redact",
		EventType:  "agent.registered",
		TargetType: "agent",
		Action:     "registered",
		Payload: map[string]any{
			// Both the token and the internal hostname must be
			// scrubbed before the prompt goes out.
			"github_token": "ghp_1234567890abcdef1234567890abcdef",
			"hostname":     "fleet01.internal",
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.RedactionSummary,
		"redaction summary should not be empty when secrets are present")
	assert.Contains(t, result.RedactionSummary, "github_token")

	// Inspect the user message the model actually received: the
	// token must NOT appear there.
	messages, _ := fake.lastBodyJSON["messages"].([]any)
	require.Len(t, messages, 1)
	msg, _ := messages[0].(map[string]any)
	userText, _ := msg["content"].(string)
	assert.NotContains(t, userText, "ghp_1234567890",
		"the raw github token must not appear in the prompt")
	assert.NotContains(t, userText, "fleet01.internal",
		"the internal hostname must not appear in the prompt")
	assert.Contains(t, userText, "<redacted:github_token>")
	assert.Contains(t, userText, "<redacted:internal_hostname>")
}

func TestExplainAuditEvent_RequiresEventID(t *testing.T) {
	fake := newFake(t, "x")
	srv := fake.start()
	defer srv.Close()
	s := mkService(t, srv.URL)

	_, err := s.ExplainAuditEvent(context.Background(), ExplainAuditEventInput{
		EventType: "x.y",
	})
	require.Error(t, err)
}

func TestExplainAuditEvent_PassesContextToPrompt(t *testing.T) {
	fake := newFake(t, "x")
	srv := fake.start()
	defer srv.Close()
	s := mkService(t, srv.URL)

	_, err := s.ExplainAuditEvent(context.Background(), ExplainAuditEventInput{
		EventID:    "evt",
		EventType:  "rollout.advanced",
		TargetType: "rollout",
		TargetID:   "r-1",
		Action:     "advanced",
		Context: map[string]string{
			"rollout.name":        "web-prod-canary",
			"rollout.state":       "running",
			"rollout.stage_index": "2 of 3",
		},
	})
	require.NoError(t, err)

	messages, _ := fake.lastBodyJSON["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	userText, _ := msg["content"].(string)
	assert.Contains(t, userText, "rollout.name")
	assert.Contains(t, userText, "web-prod-canary")
	assert.Contains(t, userText, "rollout.stage_index")
}
