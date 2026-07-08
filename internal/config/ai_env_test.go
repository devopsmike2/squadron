// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// TestApplyAIEnv pins provider selection + key/model resolution and,
// critically, that the ANTHROPIC-only default path is unchanged: with
// no SQUADRON_AI_* vars set and only ANTHROPIC_API_KEY present, the
// provider stays empty (anthropic) and the key resolves exactly as
// before — zero regression.
func TestApplyAIEnv(t *testing.T) {
	clearAIEnv := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"SQUADRON_AI_PROVIDER", "SQUADRON_AI_BASE_URL", "SQUADRON_AI_MODEL",
			"SQUADRON_AI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("anthropic default path unchanged (only ANTHROPIC_API_KEY)", func(t *testing.T) {
		clearAIEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
		c := &AIConfig{Enabled: true}
		applyAIEnv(c)
		if c.Provider != "" {
			t.Errorf("provider should stay empty (anthropic default); got %q", c.Provider)
		}
		if c.APIKeyEnv != "ANTHROPIC_API_KEY" {
			t.Errorf("APIKeyEnv: got %q, want ANTHROPIC_API_KEY", c.APIKeyEnv)
		}
		if c.APIKey != "sk-ant-xyz" {
			t.Errorf("APIKey: got %q, want sk-ant-xyz", c.APIKey)
		}
		// Models untouched — the ai package applies anthropic defaults.
		if c.ExplainModel != "" || c.MergeModel != "" {
			t.Errorf("anthropic path must not set model defaults in config; got %q/%q", c.ExplainModel, c.MergeModel)
		}
	})

	t.Run("openai via SQUADRON_AI_PROVIDER + base_url + OPENAI_API_KEY", func(t *testing.T) {
		clearAIEnv(t)
		t.Setenv("SQUADRON_AI_PROVIDER", "openai")
		t.Setenv("SQUADRON_AI_BASE_URL", "https://api.openai.com/v1")
		t.Setenv("OPENAI_API_KEY", "sk-openai-abc")
		c := &AIConfig{Enabled: true}
		applyAIEnv(c)
		if c.Provider != "openai" {
			t.Errorf("provider: got %q, want openai", c.Provider)
		}
		if c.BaseURL != "https://api.openai.com/v1" {
			t.Errorf("base_url: got %q", c.BaseURL)
		}
		if c.APIKey != "sk-openai-abc" {
			t.Errorf("APIKey: got %q, want sk-openai-abc", c.APIKey)
		}
		if c.ExplainModel != "gpt-4o-mini" || c.MergeModel != "gpt-4o" {
			t.Errorf("openai default models: got %q/%q, want gpt-4o-mini/gpt-4o", c.ExplainModel, c.MergeModel)
		}
	})

	t.Run("SQUADRON_AI_API_KEY beats OPENAI_API_KEY; SQUADRON_AI_MODEL overrides both models", func(t *testing.T) {
		clearAIEnv(t)
		t.Setenv("SQUADRON_AI_PROVIDER", "openai")
		t.Setenv("SQUADRON_AI_API_KEY", "sk-squadron-1")
		t.Setenv("OPENAI_API_KEY", "sk-openai-2")
		t.Setenv("SQUADRON_AI_MODEL", "llama3.1")
		c := &AIConfig{Enabled: true}
		applyAIEnv(c)
		if c.APIKey != "sk-squadron-1" {
			t.Errorf("SQUADRON_AI_API_KEY should win: got %q", c.APIKey)
		}
		if c.ExplainModel != "llama3.1" || c.MergeModel != "llama3.1" {
			t.Errorf("SQUADRON_AI_MODEL should override both: got %q/%q", c.ExplainModel, c.MergeModel)
		}
	})
}
