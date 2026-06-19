// Rollouts API client.

import { simpleRequest } from "./base";

import type {
  AbortCriteriaRecipe,
  Plan,
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

// v0.74 — narrow the rollouts list to one plan. Returns both
// forward and rollback steps that share the plan id.
export const listRolloutsByPlan = async (
  planID: string,
): Promise<Rollout[]> => {
  const resp = await simpleRequest<ListResponse>(
    `/rollouts?plan_id=${encodeURIComponent(planID)}`,
  );
  return resp.rollouts ?? [];
};

// v0.74 — fetch the plan envelope: shared metadata + ordered
// forward steps + rollback steps + derived state. Returns null
// on 404 so callers can render a not-found message cleanly
// instead of throwing.
export const getPlan = async (planID: string): Promise<Plan | null> => {
  try {
    return await simpleRequest<Plan>(
      `/rollouts/plans/${encodeURIComponent(planID)}`,
    );
  } catch (err) {
    // simpleRequest throws Error with a message containing the
    // status code. 404 maps to null so the caller can render a
    // dedicated empty state.
    if (err instanceof Error && err.message.includes("404")) {
      return null;
    }
    throw err;
  }
};

// v0.73 — create a plan from N rollout inputs. The first step's
// require_approval flag is honored as the plan's approval gate;
// steps 1..N are forced to false server side.
export interface CreatePlanRequest {
  steps: RolloutInput[];
}

export interface CreatePlanResponse {
  plan_id: string;
  steps: Rollout[];
  count: number;
}

export const createPlan = async (
  req: CreatePlanRequest,
): Promise<CreatePlanResponse> => {
  return simpleRequest<CreatePlanResponse>("/rollouts/plans", {
    method: "POST",
    body: JSON.stringify(req),
  });
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

// rollBackRollout — v0.60. Creates a new rollout that targets the
// source rollout's previous_config_id and links back via
// rolled_back_from_id. The source must be in a terminal state
// (succeeded, aborted, or already rolled_back). The new rollout
// flows through the normal Create path so it goes through approval
// if the source did and emits the standard rollout.created plus a
// rollout.rollback_requested audit pair. Operators reach for this
// when a completed rollout looked fine at the time but is degrading
// metrics now and they want one click to undo it.
export const rollBackRollout = async (id: string): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}/rollback`, {
    method: "POST",
    body: JSON.stringify({}),
  });
};

// approveRollout — v0.47. Transitions a rollout from pending_approval
// to pending so the engine picks it up. The actor is the authenticated
// caller (taken from the gin auth context server-side); the two-person
// rule (requester ≠ approver) is enforced in the service, so callers
// don't need to plumb the requester identity through.
export const approveRollout = async (
  id: string,
  notes?: string,
): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}/approve`, {
    method: "POST",
    body: JSON.stringify({ notes: notes ?? "" }),
  });
};

// rejectRollout — v0.47. Terminal state. The requester has to clone
// the rollout to retry. Same two-person rule as approve.
export const rejectRollout = async (
  id: string,
  notes?: string,
): Promise<Rollout> => {
  return simpleRequest<Rollout>(`/rollouts/${id}/reject`, {
    method: "POST",
    body: JSON.stringify({ notes: notes ?? "" }),
  });
};
