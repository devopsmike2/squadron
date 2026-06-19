// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v0.63 — Ask Squadron is the conversational surface. The service
// is exercised via the same fake Anthropic server pattern the other
// methods use. Three things worth verifying explicitly:
//
//  1. The system prompt the model receives contains the citation
//     rules (so a regression on the prompt is loud).
//  2. The returned answer strips the [cite:...] tags from the
//     visible text (the UI re renders them as chips).
//  3. Citations are deduplicated and order preserving — a model
//     that cites the same rollout twice should produce one chip.

func TestAsk_StripsCitationsAndDeduplicates(t *testing.T) {
	modelText := "The **web-prod-canary** rollout is paused [cite:rollout:r-abc]. " +
		"It was paused after a cost spike fired [cite:spike:s-99] earlier today. " +
		"You can resume it from the Rollouts page [cite:rollout:r-abc]."
	fake := newFake(t, modelText)
	srv := fake.start()
	defer srv.Close()

	s := mkService(t, srv.URL)
	result, err := s.Ask(context.Background(), AskInput{
		Question: "Why is the canary rollout paused?",
		Context: map[string]string{
			"rollout:r-abc": "web-prod-canary, paused, group=web-prod",
			"spike:s-99":    "cost spike on web-prod attribution=otlp_logs",
		},
		Hints: map[string]string{
			"now": "2026-06-18T14:23Z",
		},
	})
	require.NoError(t, err)

	// Citation tags must be stripped from the visible answer.
	assert.NotContains(t, result.Answer, "[cite:")
	// But the human readable claim survives.
	assert.Contains(t, result.Answer, "web-prod-canary")
	assert.Contains(t, result.Answer, "Rollouts page")

	// Two unique citations even though the model used three tags
	// (rollout:r-abc appeared twice).
	require.Len(t, result.Citations, 2)
	assert.Equal(t, AskCitation{Kind: "rollout", ID: "r-abc"}, result.Citations[0])
	assert.Equal(t, AskCitation{Kind: "spike", ID: "s-99"}, result.Citations[1])

	// Sanity check the system prompt landed.
	system, _ := fake.lastBodyJSON["system"].(string)
	assert.Contains(t, system, "operator deputy")
	assert.Contains(t, system, "[cite:kind:id]")
	assert.Contains(t, system, "Never use hyphens")
}

func TestAsk_DisabledReturnsError(t *testing.T) {
	s := NewService(Config{Enabled: false}, nil)
	_, err := s.Ask(context.Background(), AskInput{Question: "what's up?"})
	require.ErrorIs(t, err, ErrDisabled)
}

func TestAsk_RejectsEmptyQuestion(t *testing.T) {
	fake := newFake(t, "irrelevant")
	srv := fake.start()
	defer srv.Close()
	s := mkService(t, srv.URL)
	_, err := s.Ask(context.Background(), AskInput{Question: "   "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "question is required")
}

func TestAsk_EmptyContextStillAllowsAnswer(t *testing.T) {
	// The model is told to say "I don't have enough" when the bag
	// is empty. The service must still pass the call through (so
	// the model gets a chance to redirect the operator), and the
	// resulting Citations slice must be empty rather than nil so
	// the JSON marshals as [] in the API response.
	fake := newFake(t, "I don't have anything loaded for that. Try the Audit page at /audit.")
	srv := fake.start()
	defer srv.Close()

	s := mkService(t, srv.URL)
	result, err := s.Ask(context.Background(), AskInput{
		Question: "Anything I should know about?",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Answer)
	assert.NotNil(t, result.Citations)
	assert.Len(t, result.Citations, 0)
}
