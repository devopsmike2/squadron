// RBAC admin API client — role and role-binding CRUD. Enterprise-only: in OSS
// these routes 404 (callers treat a 404 as "feature unavailable"). A role
// grants a set of scoped permissions; a binding attaches a role to a principal
// (a token label or token id).

import { apiDelete, apiGet, apiPost } from "./base";

export interface Permission {
  scope: string;
  resource_type: string;
  all_resources: boolean;
  resource_ids: string[];
  // ADR 0053 — optional cluster/environment scoping. When set, the permission
  // only grants on resources whose server-observed labels match every entry
  // (AND), keyed by deployment.environment / k8s.cluster.name. Omitted for a
  // label-agnostic permission. Enterprise-enforced; ignored by the OSS
  // authorizer.
  label_match?: Record<string, string>;
}

export interface Role {
  id: string;
  name: string;
  permissions: Permission[];
}

export interface RoleInput {
  name: string;
  permissions: Permission[];
}

export interface Binding {
  id: string;
  role_id: string;
  principal_kind: string;
  principal_ref: string;
}

export interface BindingInput {
  role_id: string;
  principal_kind: string;
  principal_ref: string;
}

interface RolesResponse {
  roles: Role[];
}

interface BindingsResponse {
  bindings: Binding[];
}

export const listRoles = async (): Promise<Role[]> => {
  const resp = await apiGet<RolesResponse>("/rbac/roles");
  return resp.roles ?? [];
};

export const createRole = (input: RoleInput): Promise<Role> =>
  apiPost<Role>("/rbac/roles", input);

export const deleteRole = (id: string): Promise<void> =>
  apiDelete<void>(`/rbac/roles/${id}`);

export const listBindings = async (): Promise<Binding[]> => {
  const resp = await apiGet<BindingsResponse>("/rbac/bindings");
  return resp.bindings ?? [];
};

export const createBinding = (input: BindingInput): Promise<Binding> =>
  apiPost<Binding>("/rbac/bindings", input);

export const deleteBinding = (id: string): Promise<void> =>
  apiDelete<void>(`/rbac/bindings/${id}`);

// Catalog is the canonical authoring vocabulary served by GET /rbac/catalog in
// BOTH editions (unlike the role/binding routes, which 404 in OSS). It lets the
// role editor offer validated dropdowns instead of free-text scope/resource_type
// that an operator would otherwise have to guess (the trap issue #84 hit for
// principal_kind). Older servers that predate the endpoint 404 — callers fall
// back to free-text on any error.
export interface RBACCatalog {
  scopes: string[];
  resource_types: string[];
}

export const getCatalog = async (): Promise<RBACCatalog> => {
  const resp = await apiGet<RBACCatalog>("/rbac/catalog");
  return {
    scopes: resp.scopes ?? [],
    resource_types: resp.resource_types ?? [],
  };
};
