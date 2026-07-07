// Enterprise tamper-evidence / audit-integrity API client. Mirrors the
// auditReview client: every endpoint 404s in OSS via the seam, so callers
// feature-detect through getAuditVerifyCapabilities (a 404 surfaces as
// Error.status===404 via simpleRequest). JSON responses go through apiGet
// (auto-attaches the Bearer token + stamps error.status); the sealed
// attestation is a binary/attachment download that needs a raw bearer fetch.

import { getAuthToken } from "./auth-store";
import { apiConfig, apiGet } from "./base";

// AuditVerifyCapabilities is the non-side-effecting probe payload.
export interface AuditVerifyCapabilities {
  cross_tenant: boolean;
  fleet: boolean;
  sealed_attestation: boolean;
  patterns: string[];
}

// TenantVerification is one tenant's hash-chain verification result. ok=false
// means a break was found; first_break_seq points at the first tampered row.
export interface TenantVerification {
  tenant: string;
  ok: boolean;
  rows_verified: number;
  head_seq: number;
  head_row_hash: string;
  covers_from_seq?: number;
  first_break_seq?: number;
  detail?: string;
  verified_at?: string;
}

// FleetVerification is the cross-tenant rollup — every tenant's trail verified
// in one pass, with an aggregate ok that is false if any tenant is broken.
export interface FleetVerification {
  verified_at: string;
  ok: boolean;
  tenants: TenantVerification[];
}

export const getAuditVerifyCapabilities =
  (): Promise<AuditVerifyCapabilities> =>
    apiGet<AuditVerifyCapabilities>("/audit-verify/tenants/capabilities");

// getFleetVerify — GET /audit-verify/tenants. Verifies every tenant's trail.
export const getFleetVerify = (): Promise<FleetVerification> =>
  apiGet<FleetVerification>("/audit-verify/tenants");

// getTenantVerify — GET /audit-verify/tenants/{tenant}. Verifies one trail.
export const getTenantVerify = (tenant: string): Promise<TenantVerification> =>
  apiGet<TenantVerification>(
    `/audit-verify/tenants/${encodeURIComponent(tenant)}`,
  );

// downloadAttestation fetches the sealed attestation for one tenant as a JSON
// attachment and triggers a browser download. It goes through fetch (not a bare
// <a href>) so the Bearer token from auth-store rides along — an anchor
// navigation would drop it.
export const downloadAttestation = async (tenant: string): Promise<void> => {
  const url = `${apiConfig.baseUrl}/audit-verify/tenants/${encodeURIComponent(
    tenant,
  )}/attest?download=1`;
  const headers: Record<string, string> = {};
  const token = getAuthToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;

  const resp = await fetch(url, { headers });
  if (!resp.ok) {
    throw new Error(`attestation failed (${resp.status})`);
  }
  const blob = await resp.blob();
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const objectUrl = URL.createObjectURL(blob);
  try {
    const a = document.createElement("a");
    a.href = objectUrl;
    a.download = `attestation-${tenant}-${stamp}.json`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  } finally {
    URL.revokeObjectURL(objectUrl);
  }
};
