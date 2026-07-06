// Per-tenant usage/billing API client (ADR 0023). Enterprise-only — the routes
// 404 in OSS via the seam; callers feature-detect through getUsageCapabilities
// (a 404 surfaces as Error.status===404 via simpleRequest).

import { apiGet } from "./base";

export interface UsageCapabilities {
  cross_tenant: boolean;
  metrics: string[];
}

// UsageSummary mirrors the enterprise usage.Summary.
export interface UsageSummary {
  tenant: string;
  agents: number;
  rollouts: number;
  cross_tenant: boolean;
}

export const getUsageCapabilities = (): Promise<UsageCapabilities> =>
  apiGet<UsageCapabilities>("/usage/capabilities");

// getOwnUsage — GET /usage (the caller's own-tenant summary).
export const getOwnUsage = (): Promise<UsageSummary> =>
  apiGet<UsageSummary>("/usage");

// getTenantUsage — GET /usage/tenants/{tenant} (cross-tenant; requires
// usage:cross_tenant server-side).
export const getTenantUsage = (tenant: string): Promise<UsageSummary> =>
  apiGet<UsageSummary>(`/usage/tenants/${encodeURIComponent(tenant)}`);
