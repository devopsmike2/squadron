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
  tenant_claim: string;
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
  tenant_claim?: string;
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
  tenant_claim?: string;
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

// SCIM directory (read-only) — users and groups an IdP has provisioned into a
// tenant. Enterprise-only, like the rest of the SSO surface: these routes 404
// in OSS.
export interface DirectoryUser {
  id: string;
  external_id: string;
  user_name: string;
  email: string;
  active: boolean;
}

export interface DirectoryGroup {
  id: string;
  external_id: string;
  display_name: string;
  role_id: string;
  members: string[];
}

export const listDirectoryUsers = async (
  tenant: string,
): Promise<DirectoryUser[]> => {
  const resp = await apiGet<{ users: DirectoryUser[] }>(
    "/sso/directory/users",
    { tenant },
  );
  return resp.users ?? [];
};

export const listDirectoryGroups = async (
  tenant: string,
): Promise<DirectoryGroup[]> => {
  const resp = await apiGet<{ groups: DirectoryGroup[] }>(
    "/sso/directory/groups",
    { tenant },
  );
  return resp.groups ?? [];
};

// setGroupRole binds an IdP group (by externalId, per tenant) to an explicit
// RBAC role, overriding the create-time displayName==role-name convention
// (ADR 0019 slice 5a). The mapping persists across SCIM PATCHes and is read at
// each login. Enterprise-only route (404 in OSS).
export const setGroupRole = async (
  tenant: string,
  externalId: string,
  roleId: string,
): Promise<DirectoryGroup> =>
  apiPut<DirectoryGroup>(
    `/sso/directory/groups/${encodeURIComponent(externalId)}/role?tenant=${encodeURIComponent(tenant)}`,
    { role_id: roleId },
  );

// clearGroupRole removes a group's explicit role mapping (role_id becomes ""),
// after which the group materializes no roles until re-mapped.
export const clearGroupRole = async (
  tenant: string,
  externalId: string,
): Promise<DirectoryGroup> =>
  apiDelete<DirectoryGroup>(
    `/sso/directory/groups/${encodeURIComponent(externalId)}/role?tenant=${encodeURIComponent(tenant)}`,
  );
