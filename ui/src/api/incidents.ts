// Incident drafts API client. Move 3 of the engineer copilot
// roadmap. Five endpoints under /api/v1/incidents/drafts.

import { simpleRequest } from "./base";

import type {
  IncidentDraft,
  IncidentDraftFilter,
  PatchIncidentDraftRequest,
  PublishIncidentDraftRequest,
} from "@/types/incident";

interface ListResponse {
  drafts: IncidentDraft[];
}

export const listIncidentDrafts = async (
  filter: IncidentDraftFilter = {},
): Promise<IncidentDraft[]> => {
  const params = new URLSearchParams();
  if (filter.status) params.set("status", filter.status);
  if (filter.action_request_id)
    params.set("action_request_id", filter.action_request_id);
  if (filter.rollout_id) params.set("rollout_id", filter.rollout_id);
  const query = params.toString();
  const path = query ? `/incidents/drafts?${query}` : "/incidents/drafts";
  const resp = await simpleRequest<ListResponse>(path);
  return resp.drafts ?? [];
};

export const getIncidentDraft = (id: string): Promise<IncidentDraft> =>
  simpleRequest<IncidentDraft>(`/incidents/drafts/${id}`);

export const patchIncidentDraft = (
  id: string,
  body: PatchIncidentDraftRequest,
): Promise<IncidentDraft> =>
  simpleRequest<IncidentDraft>(`/incidents/drafts/${id}`, {
    method: "PATCH",
    body: JSON.stringify(body),
  });

export const dismissIncidentDraft = (id: string): Promise<IncidentDraft> =>
  simpleRequest<IncidentDraft>(`/incidents/drafts/${id}/dismiss`, {
    method: "POST",
    body: JSON.stringify({}),
  });

export const publishIncidentDraft = (
  id: string,
  body: PublishIncidentDraftRequest,
): Promise<IncidentDraft> =>
  simpleRequest<IncidentDraft>(`/incidents/drafts/${id}/publish`, {
    method: "POST",
    body: JSON.stringify(body),
  });
