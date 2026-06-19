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

// v0.44 — natural-language fleet query.

export interface FleetQuerySchema {
  label_keys?: string[];
  groups?: string[];
}

export interface FleetQueryRequest {
  query: string;
  schema?: FleetQuerySchema;
}

export interface FleetQueryResponse {
  status?: "online" | "offline" | "error";
  drift_status?: "synced" | "drifted" | "no_intent" | "no_effective";
  group_id?: string;
  q?: string;
  explanation: string;
  model: string;
  tokens_in: number;
  tokens_out: number;
}

export function translateFleetQuery(
  req: FleetQueryRequest,
): Promise<FleetQueryResponse> {
  return apiPost<FleetQueryResponse>("/ai/fleet-query", req);
}

// v0.44 — auto-remediate lint warnings.

export interface LintFinding {
  severity: "warning" | "error";
  code: string;
  message: string;
  path?: string;
}

export interface RemediateLintRequest {
  yaml: string;
  findings: LintFinding[];
}

export interface RemediateLintResponse {
  fixed_yaml: string;
  summary: string;
  unaddressed?: string[];
  model: string;
  tokens_in: number;
  tokens_out: number;
}

export function remediateLint(
  req: RemediateLintRequest,
): Promise<RemediateLintResponse> {
  return apiPost<RemediateLintResponse>("/ai/remediate-lint", req);
}

// v0.63 — Ask Squadron: conversational read surface. The backend
// walks recent rollouts + audit events to build a context bag,
// hands the question to the AI service, and returns a paragraph
// answer plus the rows it cited inline. The UI strips the
// [cite:kind:id] tags from the answer and renders citation chips
// in their first appearance order.

export interface AskRequest {
  question: string;
}

// AskCitation mirrors the Go struct. Kind is one of:
// rollout | agent | audit | spike | rec. v0.63 only ships rollout
// and audit as citation kinds (the bag covers those two surfaces);
// the others reserve their kind values for follow on releases so
// the UI can keep chip rendering stable.
export interface AskCitation {
  kind: "rollout" | "agent" | "audit" | "spike" | "rec";
  id: string;
  label?: string;
}

export interface AskResponse {
  answer: string;
  citations: AskCitation[];
  model: string;
  tokens_in: number;
  tokens_out: number;
}

export function askSquadron(req: AskRequest): Promise<AskResponse> {
  return apiPost<AskResponse>("/ai/ask", req);
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

// v0.84 — proposer playground.
//
// ProposerPreviewRequest mirrors the server-side wire shape. JSON
// tags on the server are snake_case; TS keys here match so the
// network request body is the literal object.
export interface ProposerPreviewRequest {
  spike_id: string;
  signal: string;
  severity: string;
  baseline_monthly_usd: number;
  peak_monthly_usd: number;
  peak_pct_above_baseline: number;
  top_agents: string[];
  top_attributes: string[];
  group_id: string;
  group_name: string;
  recent_lint_findings: string[];
  recent_recommendations: string[];
}

// ProposerPreviewResponse wraps the proposer's ProposalResult plus
// the derived cost estimate the server computes. Fields are loose
// (`unknown` for the plan / proposal payloads) on purpose — the
// playground just renders them. A future strongly-typed surface
// can refine these as the schema settles.
export interface ProposerPreviewResponse {
  declined: boolean;
  reason?: string;
  kind?: "rollout" | "plan" | "";
  proposal?: unknown;
  plan?: { steps?: unknown[] };
  reasoning?: string;
  evidence?: Array<{ kind: string; id?: string; description?: string }>;
  model?: string;
  tokens_in?: number;
  tokens_out?: number;
  estimated_usd: number;
}

export function proposerPreview(
  req: ProposerPreviewRequest,
): Promise<ProposerPreviewResponse> {
  return apiPost<ProposerPreviewResponse>("/ai/proposer/preview", req);
}
