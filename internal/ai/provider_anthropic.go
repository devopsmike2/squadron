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

// anthropicProvider speaks the Anthropic Messages API. This is the
// default backend and its Complete method is the exact HTTP body
// that used to live inline in Service.callMessages — moved verbatim
// so the default (Anthropic, only ANTHROPIC_API_KEY set) path is
// byte-for-byte unchanged.
type anthropicProvider struct {
	cfg    Config
	client *http.Client
	logger *zap.Logger
}

// anthropicRequest mirrors the Anthropic Messages API request
// shape. Kept small — we don't use tool use, streaming, or vision
// in v0.26, and the JSON marshaling can always add fields later.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the slice of the Messages API response
// shape we actually use. Other fields (id, type, role, stop
// reasons) are ignored on decode.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model string `json:"model"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete implements Provider for Anthropic. Verbatim move of the
// previous callMessages body: POST base_url + /v1/messages with the
// x-api-key + anthropic-version + Content-Type + User-Agent headers,
// the {model,max_tokens,system,messages:[{role:user,content}]} body,
// ≥400 surfaced verbatim, content[].text concatenated, usage mapped.
func (p *anthropicProvider) Complete(ctx context.Context, opts callOpts) (*callResp, error) {
	// v0.82 — opts.MaxTokens overrides the service-wide cap when set.
	// Falls back to p.cfg.MaxTokens (DefaultMaxTokens at 1024 unless
	// configured) for callers that don't need the headroom.
	maxTokens := p.cfg.MaxTokens
	if opts.MaxTokens > 0 {
		maxTokens = opts.MaxTokens
	}
	body := anthropicRequest{
		Model:     opts.Model,
		MaxTokens: maxTokens,
		System:    opts.System,
		Messages: []anthropicMessage{
			{Role: "user", Content: opts.UserText},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(p.cfg.BaseURL, "/")+"/v1/messages",
		bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.cfg.APIKey)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("User-Agent", defaultUserAgent)

	httpResp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic call: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		// Surface the Anthropic error message verbatim — it's
		// usually descriptive (rate limit, bad api key, model
		// not available) and an operator can act on it.
		return nil, fmt.Errorf("anthropic %d: %s", httpResp.StatusCode, string(truncate(raw, 500)))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if ar.Error != nil {
		return nil, fmt.Errorf("anthropic error: %s: %s", ar.Error.Type, ar.Error.Message)
	}
	if len(ar.Content) == 0 {
		return nil, errors.New("anthropic returned empty content")
	}

	// Concatenate text blocks (the API may return multiple but in
	// practice for our shapes there's one). Ignore non-text
	// blocks; we don't request tool use.
	var sb strings.Builder
	for _, block := range ar.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return &callResp{
		Text:      sb.String(),
		Model:     ar.Model,
		TokensIn:  ar.Usage.InputTokens,
		TokensOut: ar.Usage.OutputTokens,
	}, nil
}
