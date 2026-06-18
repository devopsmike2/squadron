// API client for v0.28 Realized Savings.
// Mirrors handlers/savings.go. The retrospective tracker turns the
// "$/month potential" hero on /savings into something measurable:
//   - "Saved this month" — sum of realized_savings_per_month_usd
//     over outcomes whose post-apply byte rate dropped vs. baseline.
//   - per-outcome breakdown for the audit-trail panel.

import { apiGet } from "./base";
import type { RecommendationOutcome } from "./recommendations";

export interface RealizedSavingsResponse {
  monthly_realized_usd: number;
  currency: string;
  counts: {
    realized: number;
    pending: number;
    not_observed: number;
    total: number;
  };
  outcomes: RecommendationOutcome[];
}

export function getRealizedSavings(): Promise<RealizedSavingsResponse> {
  return apiGet<RealizedSavingsResponse>("/savings/realized");
}
