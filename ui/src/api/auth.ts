// Auth API client. Wraps /api/v1/auth/tokens.

import { simpleRequest } from "./base";

export interface APIToken {
  id: string;
  label: string;
  scopes: string[]; // empty = legacy full-access (pre-v0.10 row)
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
  // Optional expiry. When set and in the past, the server rejects the
  // token at validate time (401, same as revoked).
  expires_at?: string;
}

// The canonical scope vocabulary mirrors services.AllScopes() on the
// backend. Kept in sync by hand because the list is short and stable;
// if it grows we'll consider exposing it from an endpoint.
export const ALL_SCOPES: ReadonlyArray<{
  id: string;
  label: string;
  group: string;
}> = [
  { id: "agents:read", label: "View agents", group: "Agents" },
  {
    id: "agents:write",
    label: "Modify agents (push config, restart)",
    group: "Agents",
  },
  { id: "groups:read", label: "View groups", group: "Groups" },
  { id: "groups:write", label: "Modify groups", group: "Groups" },
  { id: "configs:read", label: "View configs", group: "Configs" },
  { id: "configs:write", label: "Create / edit configs", group: "Configs" },
  { id: "telemetry:read", label: "Query telemetry", group: "Telemetry" },
  { id: "alerts:read", label: "View alert rules", group: "Alerts" },
  { id: "alerts:write", label: "Manage alert rules", group: "Alerts" },
  { id: "rollouts:read", label: "View rollouts", group: "Rollouts" },
  {
    id: "rollouts:write",
    label: "Create / abort / pause / resume rollouts",
    group: "Rollouts",
  },
  // v0.48 — separation of duties. Distinct from rollouts:write so a
  // single operator with create authority can't also approve.
  // Grant to a change-management or reviewer group only.
  {
    id: "rollouts:approve",
    label: "Approve / reject pending rollouts (two-person rule)",
    group: "Rollouts",
  },
  { id: "audit:read", label: "Read audit log + event stream", group: "Audit" },
  // v0.50 — SIEM destination management. Read = list / view (no
  // secrets ever); write = create / update / delete / test. Grant
  // write only to a small change-management group; rotating an
  // audit destination is a sensitive operation that should be
  // explicitly authorized.
  { id: "siem:read", label: "View SIEM destinations", group: "Audit" },
  {
    id: "siem:write",
    label: "Manage SIEM destinations (create / rotate / test)",
    group: "Audit",
  },
  { id: "auth:read", label: "View API tokens", group: "Auth" },
  { id: "auth:write", label: "Create / revoke API tokens", group: "Auth" },
];

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
// form on the server — losing it means issuing a new one. scopes must
// be non-empty; pass ["*"] for full access. expiresAt is optional —
// pass undefined for "never expires" or an RFC3339 timestamp for an
// auto-revoking token.
export const createAPIToken = async (
  label: string,
  scopes: string[],
  expiresAt?: string,
): Promise<CreateResponse> => {
  const body: Record<string, unknown> = { label, scopes };
  if (expiresAt) body.expires_at = expiresAt;
  return simpleRequest<CreateResponse>("/auth/tokens", {
    method: "POST",
    body: JSON.stringify(body),
  });
};

export const revokeAPIToken = async (id: string): Promise<void> => {
  await simpleRequest(`/auth/tokens/${id}/revoke`, { method: "POST" });
};
