// Rollouts API client.

import { simpleRequest } from "./base";

import type { Rollout, RolloutInput } from "@/types/rollout";

interface ListResponse {
  rollouts: Rollout[];
}

export const listRollouts = async (): Promise<Rollout[]> => {
  const resp = await simpleRequest<ListResponse>("/rollouts");
  return resp.rollouts ?? [];
};

export const getRollout = async (id: string): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}`);
};

export const createRollout = async (input: RolloutInput): Promise<Rollout> => {
  return simpleRequest<Rollout>("/rollouts", {
    method: "POST",
    body: JSON.stringify(input),
  });
};

export const abortRollout = async (
  id: string,
  reason?: string,
): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}/abort`, {
    method: "POST",
    body: JSON.stringify({ reason: reason ?? "" }),
  });
};

export const pauseRollout = async (id: string): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}/pause`, { method: "POST" });
};

export const resumeRollout = async (id: string): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}/resume`, { method: "POST" });
};
