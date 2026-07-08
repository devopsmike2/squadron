// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// OpenAI-family defaults. Applied by NewService only when
// cfg.Provider=="openai" and the field was left unset, so the
// Anthropic default path is never touched.
const (
	// DefaultOpenAIBaseURL is the public OpenAI endpoint. Any
	// OpenAI-compatible server (Azure OpenAI, Gemini's OpenAI
	// endpoint, Mistral, Ollama/vLLM/LM Studio) is reached by
	// overriding BaseURL — no per-vendor code.
	DefaultOpenAIBaseURL      = "https://api.openai.com/v1"
	DefaultOpenAIExplainModel = "gpt-4o-mini"
	DefaultOpenAIMergeModel   = "gpt-4o"
)

// openaiProvider speaks the OpenAI Chat Completions API. The single
// endpoint + BaseURL override covers OpenAI, Azure OpenAI, Google
// Gemini (OpenAI-compat), Mistral, and local Ollama/vLLM/LM Studio.
// It sits behind the same callMessages choke point as the Anthropic
// provider, so every Squadron capability works unchanged against it.
type openaiProvider struct {
	cfg    Config
	client *http.Client
	logger *zap.Logger
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []openaiMessage `json:"messages"`
}

// openaiResponse is the slice of the Chat Completions response we
// use. Other fields (id, object, created, finish_reason) are ignored.
type openaiResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete implements Provider for OpenAI-compatible endpoints. The
// Anthropic "system" field maps to a leading system message; an
// empty System omits it. Auth is Bearer by default and switches to
// the Azure api-key header when the base URL host looks like Azure.
func (p *openaiProvider) Complete(ctx context.Context, opts callOpts) (*callResp, error) {
	maxTokens := p.cfg.MaxTokens
	if opts.MaxTokens > 0 {
		maxTokens = opts.MaxTokens
	}

	// Anthropic's single system field -> OpenAI's system message.
	// Omit the system message entirely when System is empty.
	msgs := make([]openaiMessage, 0, 2)
	if strings.TrimSpace(opts.System) != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: opts.System})
	}
	msgs = append(msgs, openaiMessage{Role: "user", Content: opts.UserText})

	body := openaiRequest{
		Model:     opts.Model,
		MaxTokens: maxTokens,
		Messages:  msgs,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	base := strings.TrimRight(p.cfg.BaseURL, "/")
	if base == "" {
		base = DefaultOpenAIBaseURL
	}
	endpoint := base + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)
	// Azure OpenAI authenticates with the api-key header; every other
	// OpenAI-compatible vendor uses Authorization: Bearer.
	if strings.Contains(strings.ToLower(base), "azure") {
		req.Header.Set("api-key", p.cfg.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	httpResp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai call: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		// Surface the upstream error body verbatim, mirroring the
		// Anthropic path — usually descriptive (bad key, rate limit,
		// unknown model) and actionable by the operator.
		return nil, fmt.Errorf("openai %d: %s", httpResp.StatusCode, string(truncate(raw, 500)))
	}

	var or openaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if or.Error != nil {
		return nil, fmt.Errorf("openai error: %s: %s", or.Error.Type, or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return nil, errors.New("openai returned no choices")
	}
	return &callResp{
		Text:      or.Choices[0].Message.Content,
		Model:     or.Model,
		TokensIn:  or.Usage.PromptTokens,
		TokensOut: or.Usage.CompletionTokens,
	}, nil
}
