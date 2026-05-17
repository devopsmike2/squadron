// API client for v0.26 AI assist (Anthropic Messages API wrapper).
// Mirrors the Go service in internal/ai/ai.go — keep these in sync.
//
// Every UI surface that uses these endpoints should also probe
// /api/v1/ai/status first (via useAICapabilities below) and hide
// the affordance entirely when AI is disabled. That keeps the UI
// honest: no buttons that 503 on click.

import useSWR from "swr";

import { apiGet, apiPost } from "./base";

export interface AICapabilities {
  enabled: boolean;
  explain_model?: string;
  merge_model?: string;
}

export interface ExplainSnippetRequest {
  snippet: string;
  signal?: string;
  goal?: string;
}

export interface ExplainSnippetResponse {
  explanation: string;
  model: string;
  tokens_in: number;
  tokens_out: number;
}

export interface MergeIntoConfigRequest {
  base_yaml: string;
  snippet_yaml: string;
  goal?: string;
}

export interface MergeIntoConfigResponse {
  merged_yaml: string;
  summary: string;
  model: string;
  tokens_in: number;
  tokens_out: number;
}

export interface ExplainConfigRequest {
  yaml: string;
}

export interface ExplainConfigResponse {
  summary: string;
  pipelines?: Record<string, string>;
  model: string;
  tokens_in: number;
  tokens_out: number;
}

export function getAICapabilities(): Promise<AICapabilities> {
  return apiGet<AICapabilities>("/ai/status");
}

export function explainSnippet(
  req: ExplainSnippetRequest,
): Promise<ExplainSnippetResponse> {
  return apiPost<ExplainSnippetResponse>("/ai/explain", req);
}

export function mergeIntoConfig(
  req: MergeIntoConfigRequest,
): Promise<MergeIntoConfigResponse> {
  return apiPost<MergeIntoConfigResponse>("/ai/merge", req);
}

export function explainConfig(
  req: ExplainConfigRequest,
): Promise<ExplainConfigResponse> {
  return apiPost<ExplainConfigResponse>("/ai/explain-config", req);
}

/**
 * useAICapabilities — single shared probe of /api/v1/ai/status.
 * Cached for the whole session (5min revalidation); operators
 * don't toggle AI on/off mid-session, so chatty polling is wasted.
 *
 * Components use this to decide whether to render AI affordances
 * at all. When `loading` is true, render nothing — better than
 * flickering a button that vanishes a beat later.
 */
export function useAICapabilities(): {
  capabilities: AICapabilities | undefined;
  loading: boolean;
} {
  const { data, isLoading } = useSWR("ai-capabilities", getAICapabilities, {
    refreshInterval: 300_000,
    dedupingInterval: 60_000,
    // Failing the probe is fine — treat as disabled and move on.
    onError: () => {},
  });
  return { capabilities: data, loading: isLoading };
}
