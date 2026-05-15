// Auth API client. Wraps /api/v1/auth/tokens.

import { simpleRequest } from "./base";

export interface APIToken {
  id: string;
  label: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
}

interface ListResponse {
  tokens: APIToken[];
}

interface CreateResponse {
  token: APIToken;
  plaintext: string;
}

export const listAPITokens = async (): Promise<APIToken[]> => {
  const resp = await simpleRequest<ListResponse>("/auth/tokens");
  return resp.tokens ?? [];
};

// createAPIToken issues a token. The returned plaintext is shown to
// the operator once at creation time and never persisted in retrievable
// form on the server — losing it means issuing a new one.
export const createAPIToken = async (label: string): Promise<CreateResponse> => {
  return simpleRequest<CreateResponse>("/auth/tokens", {
    method: "POST",
    body: JSON.stringify({ label }),
  });
};

export const revokeAPIToken = async (id: string): Promise<void> => {
  await simpleRequest(`/auth/tokens/${id}/revoke`, { method: "POST" });
};
