// API client for v0.32 inventory reconciliation.
// Mirrors internal/inventory + handlers/inventory.go.

import { apiDelete, apiGet, apiPost, apiPut } from "./base";

export type InventoryStatus = "healthy" | "missing" | "unexpected";

export interface ExpectedAgent {
  hostname: string;
  labels?: Record<string, string>;
  source: string;
  expected_since: string;
  updated_at: string;
  notes?: string;
}

export interface InventoryRow {
  hostname: string;
  status: InventoryStatus;
  source?: string;
  labels?: Record<string, string>;
  notes?: string;
  last_seen?: string;
  expected_since?: string;
  agent_id?: string;
}

export interface InventoryReport {
  healthy: number;
  missing: number;
  unexpected: number;
  total: number;
  rows: InventoryRow[];
  updated_at: string;
}

export function fetchInventoryReport(source = ""): Promise<InventoryReport> {
  const q = source ? `?source=${encodeURIComponent(source)}` : "";
  return apiGet<InventoryReport>(`/inventory/reconciliation${q}`);
}

export function listExpectedAgents(
  source = "",
): Promise<{ items: ExpectedAgent[]; count: number }> {
  const q = source ? `?source=${encodeURIComponent(source)}` : "";
  return apiGet<{ items: ExpectedAgent[]; count: number }>(
    `/inventory/expected${q}`,
  );
}

export function upsertExpectedAgent(body: {
  hostname: string;
  labels?: Record<string, string>;
  source?: string;
  notes?: string;
}): Promise<ExpectedAgent> {
  return apiPost<ExpectedAgent>("/inventory/expected", body);
}

export function deleteExpectedAgent(
  hostname: string,
): Promise<{ ok: boolean; hostname: string }> {
  return apiDelete<{ ok: boolean; hostname: string }>(
    `/inventory/expected/${encodeURIComponent(hostname)}`,
  );
}

/** Bulk-rotate path for CI/CD. Replaces every row tagged with
 * `source` with the new list. Idempotent. */
export function replaceExpectedAgents(body: {
  source: string;
  entries: { hostname: string; labels?: Record<string, string>; notes?: string }[];
}): Promise<{ ok: boolean; source: string; count: number }> {
  return apiPut<{ ok: boolean; source: string; count: number }>(
    "/inventory/expected",
    body,
  );
}

export function statusColor(s: InventoryStatus): string {
  switch (s) {
    case "healthy":
      return "var(--status-healthy, #22c55e)";
    case "missing":
      return "var(--status-critical, #ef4444)";
    case "unexpected":
      return "var(--status-warn, #eab308)";
  }
}

export function statusLabel(s: InventoryStatus): string {
  switch (s) {
    case "healthy":
      return "Healthy";
    case "missing":
      return "Missing";
    case "unexpected":
      return "Unexpected";
  }
}
