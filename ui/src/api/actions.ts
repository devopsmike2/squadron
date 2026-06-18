// Action runner API client. Move 2 of the engineer copilot roadmap.
// Endpoints live under /api/v1/actions and /api/v1/runners.

import { simpleRequest } from "./base";

import type {
  ActionRequest,
  ActionRequestFilter,
  ActionRunner,
} from "@/types/action";

interface ActionListResponse {
  requests: ActionRequest[];
}

interface RunnerListResponse {
  runners: ActionRunner[];
}

export const listActionRequests = async (
  filter: ActionRequestFilter = {},
): Promise<ActionRequest[]> => {
  const params = new URLSearchParams();
  if (filter.status) params.set("status", filter.status);
  if (filter.runner_id) params.set("runner_id", filter.runner_id);
  if (filter.proposal_id) params.set("proposal_id", filter.proposal_id);
  const query = params.toString();
  const path = query ? `/actions?${query}` : "/actions";
  const resp = await simpleRequest<ActionListResponse>(path);
  return resp.requests ?? [];
};

export const getActionRequest = (id: string): Promise<ActionRequest> =>
  simpleRequest<ActionRequest>(`/actions/${id}`);

export const listActionRunners = async (): Promise<ActionRunner[]> => {
  const resp = await simpleRequest<RunnerListResponse>("/runners");
  return resp.runners ?? [];
};

export const getActionRunner = (id: string): Promise<ActionRunner> =>
  simpleRequest<ActionRunner>(`/runners/${id}`);

export const revokeActionRunner = (
  id: string,
): Promise<{ runner_id: string; revoked_at: string }> =>
  simpleRequest(`/runners/${id}/revoke`, {
    method: "POST",
    body: JSON.stringify({}),
  });
