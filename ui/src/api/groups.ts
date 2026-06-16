import type { Agent } from "../types/agent";

import { apiGet, apiPost, apiPut, apiDelete } from "./base";
import type { Config } from "./configs";

export interface Group {
  id: string;
  name: string;
  labels: Record<string, string>;
  agent_count: number;
  config_name?: string;
  // v0.48 — when true, every rollout to this group is forced into
  // pending_approval at create time regardless of what the requester
  // set on the rollout form. Surfaces as a "policy-protected" badge
  // and locks the create-form approval checkbox.
  require_approval?: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateGroupRequest {
  name: string;
  labels?: Record<string, string>;
  require_approval?: boolean;
}

// UpdateGroupRequest — partial update. Send only the fields you want
// to change; omit (or undefined) leaves the existing value untouched.
// require_approval: false IS a meaningful value (toggling policy off),
// so callers must send false explicitly rather than omit.
export interface UpdateGroupRequest {
  name?: string;
  labels?: Record<string, string>;
  require_approval?: boolean;
}

export interface AssignConfigRequest {
  config_id: string;
}

export interface AssignConfigResponse {
  message: string;
  config: Config;
}

export interface GetGroupsResponse {
  groups: Group[];
  count: number;
}

export interface GetGroupAgentsResponse {
  agents: Agent[];
  count: number;
}

// Get all groups
export const getGroups = (): Promise<GetGroupsResponse> => {
  return apiGet<GetGroupsResponse>("/groups");
};

// Get group by ID
export const getGroup = (id: string): Promise<Group> => {
  return apiGet<Group>(`/groups/${id}`);
};

// Create new group
export const createGroup = (data: CreateGroupRequest): Promise<Group> => {
  return apiPost<Group>("/groups", data);
};

// Update group — v0.48. Used by the Groups settings page to toggle
// the require_approval policy. The handler does partial updates so
// passing only { require_approval: true } leaves name/labels intact.
export const updateGroup = (
  id: string,
  data: UpdateGroupRequest,
): Promise<Group> => {
  return apiPut<Group>(`/groups/${id}`, data);
};

// Delete group
export const deleteGroup = (id: string): Promise<void> => {
  return apiDelete<void>(`/groups/${id}`);
};

// Assign config to group
export const assignConfigToGroup = (
  groupId: string,
  data: AssignConfigRequest,
): Promise<AssignConfigResponse> => {
  return apiPost<AssignConfigResponse>(`/groups/${groupId}/config`, data);
};

// Get group's active config
export const getGroupConfig = (groupId: string): Promise<Config> => {
  return apiGet<Config>(`/groups/${groupId}/config`);
};

// Get agents in group
export const getGroupAgents = (
  groupId: string,
): Promise<GetGroupAgentsResponse> => {
  return apiGet<GetGroupAgentsResponse>(`/groups/${groupId}/agents`);
};

// Restart group request/response types
export interface RestartGroupResponse {
  success: boolean;
  message: string;
  restarted_count: number;
  failed_count: number;
}

// Restart all agents in group
export const restartGroup = (
  groupId: string,
): Promise<RestartGroupResponse> => {
  return apiPost<RestartGroupResponse>(`/groups/${groupId}/restart`, {});
};
