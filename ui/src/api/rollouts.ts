// Rollouts API client.

import { simpleRequest } from "./base";

import type {
  AbortCriteriaRecipe,
  Rollout,
  RolloutInput,
  RolloutPreview,
  RolloutTemplate,
} from "@/types/rollout";

interface ListResponse {
  rollouts: Rollout[];
}

interface RecipesResponse {
  recipes: AbortCriteriaRecipe[];
}

interface TemplatesResponse {
  templates: RolloutTemplate[];
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

// listRolloutTemplates fetches the curated template gallery. Each
// template bundles stages + criteria + a default name; picking one
// prefills the entire create form except for group_id and
// target_config_id.
export const listRolloutTemplates = async (): Promise<RolloutTemplate[]> => {
  const resp = await simpleRequest<TemplatesResponse>(
    "/rollout-recipes/templates",
  );
  return resp.templates ?? [];
};

// getRolloutPreview fetches the diff + lint preview between a group's
// current effective config and a candidate target config. Called by
// the create form once both fields are set so operators see what
// they're about to ship.
export const getRolloutPreview = async (
  groupId: string,
  targetConfigId: string,
): Promise<RolloutPreview> => {
  const params = new URLSearchParams({
    group_id: groupId,
    target_config_id: targetConfigId,
  });
  return simpleRequest<RolloutPreview>(`/rollout-preview?${params.toString()}`);
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
