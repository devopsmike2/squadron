import useSWR from "swr";

import {
  getAuditVerifyCapabilities,
  type AuditVerifyCapabilities,
} from "@/api/auditVerify";

// useAuditVerifyCapabilities feature-detects the enterprise tamper-evidence
// surface by probing GET /audit-verify/tenants/capabilities. Same idiom as
// useAuditReviewCapabilities: shouldRetryOnError:false so a 404 (OSS — the
// verify seam is unmounted) is a single clean "not enterprise" signal. A
// distinct SWR key so its cache doesn't collide with the review hook.
export interface AuditVerifyCapabilitiesState {
  capabilities?: AuditVerifyCapabilities;
  isEnterprise: boolean;
  isLoading: boolean;
}

export function useAuditVerifyCapabilities(): AuditVerifyCapabilitiesState {
  const { data, error, isLoading } = useSWR<AuditVerifyCapabilities>(
    "audit-verify-capabilities",
    getAuditVerifyCapabilities,
    { shouldRetryOnError: false, revalidateOnFocus: false },
  );

  const notEnterprise =
    error != null && (error as { status?: number }).status === 404;

  return {
    capabilities: data,
    isEnterprise: data != null && !notEnterprise,
    isLoading: isLoading && error == null && data == null,
  };
}
