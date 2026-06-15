// API client for v0.42 billing connector snapshots.
// Mirrors internal/billing + handlers/billing.go.

import { apiGet } from "./base";

export interface BillingSnapshot {
  provider: string;       // "splunk" (more later)
  window: string;         // "30d" etc.
  bytes: number;
  usd?: number;
  at: string;             // ISO 8601
  source_url?: string;
}

/**
 * Returns the most recent snapshot for the configured connector, or
 * `null` when no connector is wired (the backend returns 204).
 * The Savings page uses null as the "hide the tile" signal.
 */
export async function fetchBillingSnapshot(): Promise<BillingSnapshot | null> {
  try {
    const result = await apiGet<BillingSnapshot | "">("/billing/snapshot");
    // apiGet returns "" on 204 in our implementation; treat empty
    // string as "not configured."
    if (!result || typeof result === "string") return null;
    return result;
  } catch {
    // 502 (Splunk unreachable) or 401 — the tile gracefully hides.
    return null;
  }
}

/** Human-friendly byte formatter. */
export function formatBytes(n: number): string {
  if (!n || n < 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}
