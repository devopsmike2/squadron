// SSO admin API client (ADR 0016) — OIDC connection CRUD + SCIM service-token
// minting. Enterprise-only: in OSS these routes 404 (callers treat a 404 as
// "feature unavailable"). The client_secret is write-only (Create/rotate); the
// read view never returns it.

import { apiDelete, apiGet, apiPost, apiPut } from "./base";

export interface OIDCConnection {
  id: string;
  issuer: string;
  client_id: string;
  redirect_uri: string;
  tenant_id: string;
  scopes: string[];
  default_role: string;
  display_name: string;
  active: boolean;
  created_at: string;
  updated_at: string;
}

export interface OIDCConnectionInput {
  issuer: string;
  client_id: string;
  client_secret: string; // required on create
  redirect_uri: string;
  tenant_id: string;
  scopes: string[];
  default_role: string;
  display_name: string;
  active: boolean;
}

// OIDCConnectionUpdate — omit client_secret to KEEP the existing sealed secret
// (only send it to rotate). Empty string preserves it server-side (ADR 0016).
export interface OIDCConnectionUpdate {
  issuer: string;
  client_id: string;
  client_secret?: string;
  redirect_uri: string;
  tenant_id: string;
  scopes: string[];
  default_role: string;
  display_name: string;
  active: boolean;
}

interface ListResponse {
  connections: OIDCConnection[];
}

export const listSSOConnections = async (): Promise<OIDCConnection[]> => {
  const resp = await apiGet<ListResponse>("/sso/connections");
  return resp.connections ?? [];
};

export const getSSOConnection = (id: string): Promise<OIDCConnection> =>
  apiGet<OIDCConnection>(`/sso/connections/${id}`);

export const createSSOConnection = (
  input: OIDCConnectionInput,
): Promise<OIDCConnection> =>
  apiPost<OIDCConnection>("/sso/connections", input);

export const updateSSOConnection = (
  id: string,
  input: OIDCConnectionUpdate,
): Promise<OIDCConnection> =>
  apiPut<OIDCConnection>(`/sso/connections/${id}`, input);

export const deleteSSOConnection = (id: string): Promise<void> =>
  apiDelete<void>(`/sso/connections/${id}`);

export interface SCIMTokenInput {
  tenant_id: string;
  description?: string;
  expires_at?: string;
}

export interface SCIMTokenResult {
  token_id: string;
  plaintext: string; // shown once
  label: string;
  tenant_id: string;
  scopes: string[];
}

// mintSCIMToken issues a scim:-labelled, tenant-bound service token. The
// plaintext is returned ONCE (ADR 0016) — the caller must surface it for the
// operator to copy into their IdP.
export const mintSCIMToken = (
  input: SCIMTokenInput,
): Promise<SCIMTokenResult> =>
  apiPost<SCIMTokenResult>("/sso/scim-tokens", input);
