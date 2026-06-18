// Wire types for the incident drafter. Mirrors
// types.IncidentDraft and the request bodies in
// internal/api/handlers/incidents.go.

export type IncidentDraftStatus = "draft" | "published" | "dismissed";

export type IncidentProvider =
  | "clipboard"
  | "github"
  | "linear"
  | "jira"
  | "generic";

export interface IncidentDraft {
  id: string;
  action_request_id?: string;
  rollout_id?: string;
  status: IncidentDraftStatus;
  title: string;
  body_markdown: string;
  draft_content_json?: string;
  provider?: IncidentProvider;
  external_id?: string;
  external_url?: string;
  created_at: string;
  updated_at: string;
}

export interface IncidentDraftFilter {
  status?: IncidentDraftStatus;
  action_request_id?: string;
  rollout_id?: string;
}

export interface PatchIncidentDraftRequest {
  title?: string;
  body_markdown?: string;
}

export interface PublishIncidentDraftRequest {
  provider: IncidentProvider;
  external_id?: string;
  external_url?: string;
}
