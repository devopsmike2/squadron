// Tenant admin API client — tenant CRUD plus per-tenant token assignment.
// Enterprise-only: in OSS these routes 404 (callers treat a 404 as "feature
// unavailable"). A tenant owns API tokens; deleting a tenant that still owns
// tokens is guarded server-side (400 with a detail the caller surfaces).

import { apiDelete, apiGet, apiPost } from "./base";

export interface Tenant {
  tenant_id: string;
  name: string;
  created_at: string;
}

export interface TenantInput {
  name: string;
  tenant_id?: string;
}

interface TenantsResponse {
  tenants: Tenant[];
}

interface TenantTokensResponse {
  tenant_id: string;
  token_ids: string[];
}

export const listTenants = async (): Promise<Tenant[]> => {
  const resp = await apiGet<TenantsResponse>("/tenants");
  return resp.tenants ?? [];
};

export const createTenant = (input: TenantInput): Promise<Tenant> =>
  apiPost<Tenant>("/tenants", input);

export const deleteTenant = (id: string): Promise<void> =>
  apiDelete<void>(`/tenants/${id}`);

export const listTenantTokens = async (id: string): Promise<string[]> => {
  const resp = await apiGet<TenantTokensResponse>(`/tenants/${id}/tokens`);
  return resp.token_ids ?? [];
};

export const assignTenantToken = (
  id: string,
  tokenId: string,
): Promise<{ assigned: boolean }> =>
  apiPost<{ assigned: boolean }>(`/tenants/${id}/tokens`, {
    token_id: tokenId,
  });
