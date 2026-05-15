// Rollouts API client.

import { simpleRequest } from "./base";

import type {
  AbortCriteriaRecipe,
  Rollout,
  RolloutInput,
} from "@/types/rollout";

interface ListResponse {
  rollouts: Rollout[];
}

interface RecipesResponse {
  recipes: AbortCriteriaRecipe[];
}

// listAbortCriteriaRecipes fetches the curated cookbook of abort-criteria
// recipes from the server. The list is small and only changes on server
// upgrade, so callers can cache it for the lifetime of the page.
export const listAbortCriteriaRecipes = async (): Promise<
  AbortCriteriaRecipe[]
> => {
  const resp = await simpleRequest<RecipesResponse>(
    "/rollout-recipes/abort-criteria",
  );
  return resp.recipes ?? [];
};

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
