import { apiDelete, apiGet, apiPatch, apiPost } from "./base";

import type { Agent, AgentStats } from "@/types/agent";

/**
 * GET /api/v1/agents response.
 *
 * v0.23 added `items` + pagination envelope. `agents` (map),
 * `totalCount`, `activeCount`, and `inactiveCount` remain for
 * back-compat — pages built before pagination existed still work.
 */
export interface GetAgentsResponse {
  // v0.23+ paginated shape.
  items: Agent[];
  total: number;
  offset: number;
  limit: number;

  // Legacy back-compat fields.
  agents: Record<string, Agent>;
  totalCount: number;
  activeCount: number;
  inactiveCount: number;
}

export interface GetAgentsParams {
  offset?: number;
  limit?: number;
  drift_status?: string;
  status?: string;
  group_id?: string;
  q?: string;
}

/**
 * Build a query string from the params. Empty / undefined values
 * are skipped so the URL stays clean (and the server's "any"
 * branches don't get tickled with an empty string).
 */
function buildAgentsQuery(params?: GetAgentsParams): string {
  if (!params) return "";
  const usp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === "" || v === null) continue;
    usp.set(k, String(v));
  }
  const q = usp.toString();
  return q ? `?${q}` : "";
}

// Get a (paginated, filtered) page of agents. Default page size
// matches the server's defaultAgentsLimit.
export const getAgents = (
  params?: GetAgentsParams,
): Promise<GetAgentsResponse> => {
  return apiGet<GetAgentsResponse>(`/agents${buildAgentsQuery(params)}`);
};

// Get agent by ID
export const getAgent = (id: string): Promise<Agent> => {
  return apiGet<Agent>(`/agents/${id}`);
};

// Get agent statistics
export const getAgentStats = (): Promise<AgentStats> => {
  return apiGet<AgentStats>("/agents/stats");
};

// Update agent group. Pass a group id to (re)assign membership, or
// null to clear it (the backend normalizes null and "" identically to
// "no group"). This is the explicit-membership counterpart to config
// resolution: attaching a config to a group creates zero membership,
// so operators move agents in and out of groups through this call.
export const updateAgentGroup = (
  id: string,
  groupId: string | null,
): Promise<void> => {
  return apiPatch<void>(`/agents/${id}/group`, { group_id: groupId });
};

// Send configuration to agent request/response types
export interface SendConfigToAgentRequest {
  content: string;
}

export interface SendConfigToAgentResponse {
  success: boolean;
  message: string;
  config_id?: string;
}

// Send configuration to agent
export const sendConfigToAgent = (
  agentId: string,
  content: string,
): Promise<SendConfigToAgentResponse> => {
  return apiPost<SendConfigToAgentResponse>(`/agents/${agentId}/config`, {
    content,
  });
};

// Restart agent request/response types
export interface RestartAgentResponse {
  success: boolean;
  message: string;
}

// Restart agent
export const restartAgent = (
  agentId: string,
): Promise<RestartAgentResponse> => {
  return apiPost<RestartAgentResponse>(`/agents/${agentId}/restart`, {});
};

// Adopt an agent's reported effective config as a managed template.
export interface AdoptConfigRequest {
  /** Optional name override; defaults to "<agent>-adopted" server-side. */
  name?: string;
}

export interface AdoptedConfig {
  id: string;
  name: string;
  agent_id?: string;
  group_id?: string;
  config_hash: string;
  content: string;
  version: number;
  created_at: string;
}

export interface AdoptConfigResponse {
  config: AdoptedConfig;
  /** true when the reported effective config still carried literal
   * redaction markers; the config was created verbatim but the operator
   * should swap markers for ${ENV} references before assigning. */
  redacted: boolean;
  note?: string;
  warnings?: string[];
}

/**
 * Adopt the agent's reported effective config as a standalone managed
 * Config template. The new config is created unassigned and is not
 * pushed anywhere — the operator assigns it afterward via the normal
 * flow. Returns the created config plus a redaction advisory.
 */
export const adoptAgentConfig = (
  agentId: string,
  body?: AdoptConfigRequest,
): Promise<AdoptConfigResponse> => {
  return apiPost<AdoptConfigResponse>(
    `/agents/${agentId}/adopt-config`,
    body ?? {},
  );
};

/** v0.35: hard-delete an agent record. Used for retiring physically
 * decommissioned hosts so they don't pollute the inventory view
 * forever. Telemetry already written for that agent stays in DuckDB. */
export const decommissionAgent = (
  agentId: string,
): Promise<{ ok: boolean; agent_id: string }> => {
  return apiDelete<{ ok: boolean; agent_id: string }>(`/agents/${agentId}`);
};

/** Backlog #5: dismiss the suspected-duplicate flag on a telemetry-only agent,
 * confirming it is a legitimate separate agent (not a phantom of an
 * OpAMP-managed host). Clears the warning badge/banner and persists the
 * decision so it stays cleared. Does not delete or move any telemetry. */
export const dismissDuplicate = (agentId: string): Promise<Agent> => {
  return apiPost<Agent>(`/agents/${agentId}/dismiss-duplicate`, {});
};
