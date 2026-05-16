import { apiGet, apiPatch, apiPost } from "./base";

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
export const getAgents = (params?: GetAgentsParams): Promise<GetAgentsResponse> => {
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

// Update agent group
export const updateAgentGroup = (
  id: string,
  groupId: string,
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
