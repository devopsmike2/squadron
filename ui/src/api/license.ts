// License status API client (ADR 0032 S1). GET /api/v1/license/status is always
// served: OSS returns {edition:"oss"}; the enterprise edition returns the
// guard-backed view.

import { apiGet } from "./base";

export interface LicenseLimits {
  max_agents: number;
  max_admin_seats: number;
  max_tenants: number;
}

export interface LicenseStatus {
  edition: "oss" | "enterprise";
  state: string; // valid | grace | expired | not_yet_valid | invalid | n/a
  development?: boolean;
  customer?: string;
  plan?: string;
  trial?: boolean;
  expires_at?: string;
  days_remaining?: number;
  in_grace?: boolean;
  features?: string[];
  limits?: LicenseLimits;
  message?: string;
}

export const getLicenseStatus = (): Promise<LicenseStatus> =>
  apiGet<LicenseStatus>("/license/status");

// UPGRADE_URL is the "Upgrade / Manage license" CTA destination. Configurable via
// the VITE_ENTERPRISE_UPGRADE_URL env value so it can be repointed (pricing page,
// contact form, Calendly) with no code change; defaults to the marketing page.
export const UPGRADE_URL: string =
  (import.meta.env.VITE_ENTERPRISE_UPGRADE_URL as string | undefined) ??
  "https://squadron.dev/enterprise";
