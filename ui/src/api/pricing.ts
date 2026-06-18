// API client for v0.27 Pricing projection.
// Mirrors the Go shapes in internal/pricing/pricing.go. Keep in sync.

import { apiGet } from "./base";
import type { InsightsSignal, InsightsWindow } from "./insights";

export interface PricingRule {
  match: string; // empty = catch-all
  label?: string;
  price_per_gb: number;
  traces?: number;
  metrics?: number;
  logs?: number;
}

export interface PricingConfig {
  enabled: boolean;
  currency: string;
  rules: PricingRule[];
}

export interface PricingProjection {
  enabled: boolean;
  currency: string;
  monthly_usd: number;
  /** Per-signal $/month at the fleet-wide catch-all rate. Absent
   * when there's no volume in that window. */
  by_signal?: Partial<Record<InsightsSignal, number>>;
  assumptions?: PricingRule[];
  window?: InsightsWindow;
  agent_count?: number;
}

export function getPricingConfig(): Promise<PricingConfig> {
  return apiGet<PricingConfig>("/pricing/config");
}

export function getPricingProjection(
  window: InsightsWindow = "24h",
): Promise<PricingProjection> {
  return apiGet<PricingProjection>(`/pricing/projection?window=${window}`);
}

// v0.39.0 — Month-end spend forecast. The backend pro-rates the
// steady-state monthly figure across the actual calendar month and
// splits it into elapsed + remaining. The UI uses this to render a
// "projected $X by EOM" tile next to the existing "estimated
// monthly" tile, plus a delta vs the rate.
export interface PricingForecast {
  enabled: boolean;
  currency: string;
  /** Steady-state monthly USD at current rate, 30-day baseline. */
  steady_state_usd?: number;
  /** Adjusted to actual days in this calendar month. */
  forecast_usd?: number;
  /** Spend already incurred this month at current rate. */
  spent_so_far_usd?: number;
  /** Remaining spend until end of month. */
  remaining_usd?: number;
  /** 0..1 — share of the month already elapsed by seconds. */
  fraction_elapsed?: number;
  days_in_month?: number;
  days_elapsed?: number;
  calendar_month?: string;
  agent_count?: number;
}

export function getPricingForecast(): Promise<PricingForecast> {
  return apiGet<PricingForecast>(`/pricing/forecast`);
}

/**
 * Match a destination_key against a rule set, returning the first
 * rule whose `match` is a substring of the key. Catch-all (empty
 * match) is the fallback. Mirrors pricing.Projector.matchRule in Go
 * so the UI can do per-destination $ math without a round-trip.
 */
export function matchPricingRule(
  destinationKey: string,
  rules: PricingRule[],
): PricingRule {
  const key = destinationKey.toLowerCase();
  for (const r of rules) {
    if (r.match === "") return r;
    if (key.includes(r.match.toLowerCase())) return r;
  }
  // Defensive fallback if no catch-all is present.
  return { match: "", price_per_gb: 0.3 };
}

/**
 * Compute $/month for a byte count observed in `windowSeconds`,
 * priced at the rule's signal-specific or base rate. Mirrors
 * pricing.Projector.MonthlyForBytesWindow so the UI can build the
 * per-destination breakdown locally.
 */
export function monthlyUSDFor(
  bytesInWindow: number,
  windowSeconds: number,
  signal: InsightsSignal | "" | undefined,
  rule: PricingRule,
): number {
  if (bytesInWindow <= 0 || windowSeconds <= 0) return 0;
  const bytesPerHour = (bytesInWindow * 3600) / windowSeconds;
  const bytesPerGB = 1024 * 1024 * 1024;
  const hoursPerMonth = 730;
  const gbPerMonth = (bytesPerHour * hoursPerMonth) / bytesPerGB;
  let rate = rule.price_per_gb;
  if (signal === "traces" && rule.traces) rate = rule.traces;
  else if (signal === "metrics" && rule.metrics) rate = rule.metrics;
  else if (signal === "logs" && rule.logs) rate = rule.logs;
  return gbPerMonth * rate;
}

/** Locale-aware USD formatter. */
export function formatUSD(amount: number): string {
  if (!isFinite(amount)) return "$0";
  if (amount >= 10000) {
    return new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: "USD",
      maximumFractionDigits: 0,
    }).format(amount);
  }
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(amount);
}
