// Per-tenant trace-index budget admin API client. Enterprise-only — the routes
// 404 in OSS via the seam; callers feature-detect through getBudgetCapabilities
// (a 404 surfaces as Error.status===404 via simpleRequest). A budget caps the
// number of trace-index rows a tenant may retain; max_rows of 0 means "no
// override" (the global cap applies).

import { apiGet, apiPut, apiDelete } from "./base";

export interface BudgetCapabilities {
  cross_tenant: boolean;
  scopes: string[];
}

// Budget mirrors the enterprise budgets.Budget row.
export interface Budget {
  tenant: string;
  max_rows: number;
  updated_at: string;
}

export const getBudgetCapabilities = (): Promise<BudgetCapabilities> =>
  apiGet<BudgetCapabilities>("/budgets/capabilities");

// listBudgets — GET /budgets. Returns the caller's own-tenant budget row, or
// ALL tenants' budgets when the caller holds budgets:cross_tenant.
export const listBudgets = async (): Promise<Budget[]> =>
  (await apiGet<{ budgets: Budget[] }>("/budgets")).budgets ?? [];

// getTenantBudget — GET /budgets/tenants/{tenant} (cross-tenant read).
export const getTenantBudget = (tenant: string): Promise<Budget> =>
  apiGet<Budget>(`/budgets/tenants/${encodeURIComponent(tenant)}`);

// putTenantBudget — PUT /budgets/tenants/{tenant} (requires budgets:write
// server-side; max_rows must be positive).
export const putTenantBudget = (
  tenant: string,
  maxRows: number,
): Promise<Budget> =>
  apiPut<Budget>(`/budgets/tenants/${encodeURIComponent(tenant)}`, {
    max_rows: maxRows,
  });

// deleteTenantBudget — DELETE /budgets/tenants/{tenant} (204).
export const deleteTenantBudget = (tenant: string): Promise<void> =>
  apiDelete<void>(`/budgets/tenants/${encodeURIComponent(tenant)}`);
